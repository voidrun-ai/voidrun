package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"voidrun/internal/model"
	"voidrun/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	maxPathLength    = 4096
	maxPatternLength = 1024
	maxModeLength    = 10
	maxLines         = 10000
)

// validatePath checks for dangerous path patterns
func validatePath(path string) error {
	if len(path) > maxPathLength {
		return fmt.Errorf("path exceeds maximum length")
	}
	if path == "" {
		return fmt.Errorf("path is required")
	}
	if strings.Contains(path, "\x00") {
		return fmt.Errorf("path contains null bytes")
	}
	return nil
}

// sanitizeFilename removes dangerous characters from filenames for Content-Disposition
func sanitizeFilename(name string) string {
	// Remove path separators and null bytes
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "\x00", "")
	// Remove quotes to prevent header injection
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "'", "")
	return name
}

// FSHandler handles filesystem operations
type FSHandler struct {
	fsService      *service.FSService
	sandboxService *service.SandboxService
}

// Shared 64KB Buffer Pool
var bufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 64*1024)
		return &b
	},
}

// HandleJSONResponse normalizes agent responses into JSON payloads for clients.
// If the agent returns JSON, it is forwarded verbatim with the original status.
// Otherwise, success responses become a simple {success:true,message:"..."}, and
// error responses wrap the agent body in our standard error envelope.
func HandleJSONResponse(c *gin.Context, resp *http.Response) {
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to read sandbox response", err.Error()))
		return
	}

	contentType := resp.Header.Get("Content-Type")
	status := resp.StatusCode

	// Prefer pass-through of JSON without decoding; wrap in our standard envelope using RawMessage
	if strings.Contains(contentType, "application/json") {
		raw := json.RawMessage(bodyBytes)
		if status >= 400 {
			c.JSON(status, model.NewErrorResponse("Sandbox error", string(bodyBytes)))
		} else {
			c.JSON(status, model.NewSuccessResponse("ok", raw))
		}
		return
	}

	bodyStr := strings.TrimSpace(string(bodyBytes))
	if status >= 400 {
		c.JSON(status, model.NewErrorResponse("Sandbox error", bodyStr))
		return
	}

	c.JSON(status, model.NewSuccessResponse(bodyStr, nil))
}

// NewFSHandler creates a new filesystem handler
func NewFSHandler(fsService *service.FSService, sandboxService *service.SandboxService) *FSHandler {
	return &FSHandler{
		fsService:      fsService,
		sandboxService: sandboxService,
	}
}

// streamCopy copies from src to dst using the shared buffer pool
func (h *FSHandler) streamCopy(dst io.Writer, src io.Reader) (int64, error) {
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	return io.CopyBuffer(dst, src, *buf)
}

// ListFiles handles GET /sandboxes/:id/fs?path=/path/to/dir
func (h *FSHandler) ListFiles(c *gin.Context) {
	id := c.Param("id")
	path := c.DefaultQuery("path", "/root")

	if err := validatePath(path); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid path", err.Error()))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.ListFiles(c.Request.Context(), sbxInstance, path)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to list files", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// DownloadFile handles GET /sandboxes/:id/fs/download?path=/path/to/file
func (h *FSHandler) DownloadFile(c *gin.Context) {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path is required", ""))
		return
	}

	if err := validatePath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid path", err.Error()))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.DownloadFile(c.Request.Context(), sbxInstance, filePath)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to download file", err.Error()))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.Status(resp.StatusCode)
		h.streamCopy(c.Writer, resp.Body)
		return
	}

	// Set download headers
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	c.Header("Content-Type", "application/octet-stream")
	safeFilename := sanitizeFilename(filepath.Base(filePath))
	c.Header("Content-Disposition", "attachment; filename=\""+safeFilename+"\"")
	c.Status(http.StatusOK)

	io.Copy(c.Writer, resp.Body)
}

// UploadFile handles POST /sandboxes/:id/fs/upload?path=/path/to/file
func (h *FSHandler) UploadFile(c *gin.Context) {
	id := c.Param("id")
	targetPath := c.Query("path")

	if targetPath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path is required", ""))
		return
	}

	if err := validatePath(targetPath); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid path", err.Error()))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	var bodyReader io.Reader
	var contentLength string
	var contentType string

	// Handle multipart form-data (Postman, browser forms)
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		const maxSize = 5 // 5 MB

		// Reject large uploads early
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSize<<20)

		fileHeader, err := c.FormFile("file")
		if err != nil {
			if strings.Contains(err.Error(), "request body too large") {
				c.JSON(http.StatusRequestEntityTooLarge, model.NewErrorResponse(
					fmt.Sprintf("File too large. Maximum %dMB for multipart uploads. Use binary upload for larger files.", maxSize),
					"",
				))
				return
			}

			c.JSON(http.StatusBadRequest, model.NewErrorResponse(
				"No file found in multipart upload. Expected field name 'file'",
				err.Error(),
			))
			return
		}

		file, err := fileHeader.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to open uploaded file", err.Error()))
			return
		}
		defer file.Close()

		bodyReader = file
		contentLength = fmt.Sprintf("%d", fileHeader.Size)
		contentType = fileHeader.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		log.Printf("[FS] Multipart upload: %s, size: %s, type: %s", fileHeader.Filename, contentLength, contentType)
	} else {
		// Raw binary upload (fetch, XHR, curl --data-binary)
		bodyReader = c.Request.Body
		contentLength = c.Request.Header.Get("Content-Length")
		contentType = c.Request.Header.Get("Content-Type")

		log.Printf("[FS] Binary upload: path: %s, size: %s, type: %s", targetPath, contentLength, contentType)
	}

	resp, err := h.fsService.UploadFile(
		c.Request.Context(),
		sbxInstance,
		targetPath,
		bodyReader,
		contentLength,
		contentType,
	)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Upload failed", err.Error()))
		return
	}
	defer resp.Body.Close()

	c.Status(resp.StatusCode)
	h.streamCopy(c.Writer, resp.Body)
}

// DeleteFile handles DELETE /sandboxes/:id/fs?path=/path/to/file
func (h *FSHandler) DeleteFile(c *gin.Context) {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path is required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.DeleteFile(c.Request.Context(), sbxInstance, filePath)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to delete file", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// CreateDirectory handles POST /sandboxes/:id/fs/mkdir?path=/path/to/dir
func (h *FSHandler) CreateDirectory(c *gin.Context) {
	id := c.Param("id")
	dirPath := c.Query("path")
	if dirPath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path is required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.CreateDirectory(c.Request.Context(), sbxInstance, dirPath)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to create directory", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// MoveFile handles POST /sandboxes/:id/fs/move?from=/path/from&to=/path/to
func (h *FSHandler) MoveFile(c *gin.Context) {
	id := c.Param("id")
	sourcePath := c.Query("from")
	destPath := c.Query("to")

	if sourcePath == "" || destPath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("from and to query params are required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.MoveFile(c.Request.Context(), sbxInstance, sourcePath, destPath)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to move file", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// CreateFile handles POST /sandboxes/:id/files/create?path=/path/to/file
func (h *FSHandler) CreateFile(c *gin.Context) {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path is required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.CreateFile(c.Request.Context(), sbxInstance, filePath)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to create file", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// StatFile handles GET /sandboxes/:id/fs/stat?path=/path/to/file
func (h *FSHandler) StatFile(c *gin.Context) {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path is required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.StatFile(c.Request.Context(), sbxInstance, filePath)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to get file info", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// CopyFile handles POST /sandboxes/:id/files/copy?from=...&to=...
func (h *FSHandler) CopyFile(c *gin.Context) {
	id := c.Param("id")
	from := c.Query("from")
	to := c.Query("to")
	if from == "" || to == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("from and to required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.CopyFile(c.Request.Context(), sbxInstance, from, to)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to copy file", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// HeadTail handles GET /sandboxes/:id/files/head-tail?path=...&lines=10&head=true
func (h *FSHandler) HeadTail(c *gin.Context) {
	id := c.Param("id")
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path required", ""))
		return
	}

	if err := validatePath(path); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid path", err.Error()))
		return
	}

	lines := 10
	if l := c.Query("lines"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			if parsed > maxLines {
				lines = maxLines
			} else {
				lines = parsed
			}
		}
	}

	isHead := c.DefaultQuery("head", "true") == "true"

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.HeadTail(c.Request.Context(), sbxInstance, path, lines, isHead)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to read file", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// ChangePermissions handles POST /sandboxes/:id/files/chmod?path=...&mode=755
func (h *FSHandler) ChangePermissions(c *gin.Context) {
	id := c.Param("id")
	path := c.Query("path")
	mode := c.Query("mode")
	if path == "" || mode == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path and mode required", ""))
		return
	}

	if err := validatePath(path); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid path", err.Error()))
		return
	}

	if len(mode) > maxModeLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("mode exceeds maximum length", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.ChangePermissions(c.Request.Context(), sbxInstance, path, mode)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to change permissions", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// DiskUsage handles GET /sandboxes/:id/files/du?path=...
func (h *FSHandler) DiskUsage(c *gin.Context) {
	id := c.Param("id")
	path := c.Query("path")
	if path == "" {
		path = "/root"
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.DiskUsage(c.Request.Context(), sbxInstance, path)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to get disk usage", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// SearchFiles handles GET /sandboxes/:id/files/search?path=...&pattern=...
func (h *FSHandler) SearchFiles(c *gin.Context) {
	id := c.Param("id")
	path := c.Query("path")
	pattern := c.Query("pattern")
	if path == "" {
		path = "/root"
	}
	if pattern == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("pattern required", ""))
		return
	}

	if err := validatePath(path); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid path", err.Error()))
		return
	}

	if len(pattern) > maxPatternLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("pattern exceeds maximum length", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.SearchFiles(c.Request.Context(), sbxInstance, path, pattern)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to search files", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// CompressFile handles POST /sandboxes/:id/files/compress?path=...&format=tar.gz
func (h *FSHandler) CompressFile(c *gin.Context) {
	id := c.Param("id")
	path := c.Query("path")
	format := c.Query("format")
	if path == "" || format == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("path and format required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.CompressFile(c.Request.Context(), sbxInstance, path, format)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to compress file", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// ExtractArchive handles POST /sandboxes/:id/files/extract?archive=...&dest=...
func (h *FSHandler) ExtractArchive(c *gin.Context) {
	id := c.Param("id")
	archive := c.Query("archive")
	dest := c.Query("dest")
	if archive == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("archive required", ""))
		return
	}
	if dest == "" {
		dest = filepath.Dir(archive)
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	resp, err := h.fsService.ExtractArchive(c.Request.Context(), sbxInstance, archive, dest)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to extract archive", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// StartWatch handles POST /sandboxes/:id/files/watch/start
func (h *FSHandler) StartWatch(c *gin.Context) {
	id := c.Param("id")

	var req struct {
		Path         string `json:"path" binding:"required"`
		Recursive    bool   `json:"recursive"`
		IgnoreHidden *bool  `json:"ignoreHidden"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request body", err.Error()))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	ignoreHidden := true
	if req.IgnoreHidden != nil {
		ignoreHidden = *req.IgnoreHidden
	}

	resp, err := h.fsService.StartWatch(c.Request.Context(), sbxInstance, req.Path, req.Recursive, ignoreHidden)
	if err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Failed to start watch", err.Error()))
		return
	}
	defer resp.Body.Close()

	var agentResp struct {
		Success   bool   `json:"success"`
		Error     string `json:"error"`
		SessionID string `json:"sessionId"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Invalid sandbox response", err.Error()))
		return
	}

	if !agentResp.Success {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Sandbox error", agentResp.Error))
		return
	}

	if agentResp.SessionID == "" {
		c.JSON(http.StatusBadGateway, model.NewErrorResponse("Sandbox error", "missing sessionId"))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("watch started", map[string]interface{}{
		"sessionId": agentResp.SessionID,
	}))
}

// StreamWatchEvents handles WebSocket streaming of file watch events
func (h *FSHandler) StreamWatchEvents(c *gin.Context) {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	if sessionID == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("sessionId is required", ""))
		return
	}

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	// Upgrade client connection to WebSocket
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // TODO: Implement proper origin checking
		},
	}

	clientConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[Watch] Failed to upgrade client connection: %v", err)
		return
	}
	defer clientConn.Close()

	// Connect to agent's WebSocket stream
	agentURL := fmt.Sprintf("ws://%s/watch/stream?sessionId=%s", sbxInstance, sessionID)

	dialer := service.NewVsockWSDialer()
	agentConn, _, err := dialer.DialContext(c.Request.Context(), agentURL, nil)
	if err != nil {
		log.Printf("[Watch] Failed to connect to agent: %v", err)
		clientConn.WriteJSON(map[string]string{"error": "Failed to connect to watch session"})
		return
	}
	defer agentConn.Close()

	log.Printf("[Watch] Streaming events from session %s to client", sessionID)

	// Bidirectional relay
	var wg sync.WaitGroup
	wg.Add(2)

	// Agent -> Client
	go func() {
		defer wg.Done()
		for {
			var event map[string]interface{}
			if err := agentConn.ReadJSON(&event); err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("[Watch] Agent connection closed: %v", err)
				}
				return
			}
			log.Printf("[Watch] Forwarding event for session %s: %v", sessionID, event)
			if err := clientConn.WriteJSON(event); err != nil {
				log.Printf("[Watch] Failed to send to client: %v", err)
				return
			}
		}
	}()

	// Client -> Agent (for ping/pong to detect disconnect)
	go func() {
		defer wg.Done()
		for {
			if _, _, err := clientConn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	wg.Wait()
	log.Printf("[Watch] Stream closed for session %s", sessionID)
}
