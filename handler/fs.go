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

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

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
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "'", "")
	return name
}

// FSHandler handles filesystem operations
type FSHandler struct {
	fsService      *service.FSService
	sandboxService *service.SandboxService
	dialer         *service.VsockWSDialer
}

// Shared 64KB Buffer Pool
var bufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 64*1024)
		return &b
	},
}
 
// NewFSHandler creates a new filesystem handler
func NewFSHandler(fsService *service.FSService, sandboxService *service.SandboxService, dialer *service.VsockWSDialer) *FSHandler {
	return &FSHandler{
		fsService:      fsService,
		sandboxService: sandboxService,
		dialer:         dialer,
	}
}

// streamCopy copies from src to dst using the shared buffer pool
func (h *FSHandler) streamCopy(dst io.Writer, src io.Reader) (int64, error) {
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	return io.CopyBuffer(dst, src, *buf)
}

// ListFiles handles GET /sandboxes/:id/fs?path=/path/to/dir
func (h *FSHandler) ListFiles(c *gin.Context) error {
	id := c.Param("id")
	path := c.DefaultQuery("path", "/root")

	if err := validatePath(path); err != nil {
		return util.ErrBadRequest("Invalid path", err.Error())
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.ListFiles(c.Request.Context(), id, path)
	if err != nil {
		return util.ErrBadGateway("Failed to list files", err)
	}
	return HandleJSONResponse(c, resp)
}

// DownloadFile handles GET /sandboxes/:id/fs/download?path=/path/to/file
func (h *FSHandler) DownloadFile(c *gin.Context) error {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		return util.ErrBadRequest("path is required")
	}
	if err := validatePath(filePath); err != nil {
		return util.ErrBadRequest("Invalid path", err.Error())
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.DownloadFile(c.Request.Context(), id, filePath)
	if err != nil {
		return util.ErrBadGateway("Failed to download file", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.Status(resp.StatusCode)
		h.streamCopy(c.Writer, resp.Body)
		return nil
	}

	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	c.Header("Content-Type", "application/octet-stream")
	safeFilename := sanitizeFilename(filepath.Base(filePath))
	c.Header("Content-Disposition", "attachment; filename=\""+safeFilename+"\"")
	c.Status(http.StatusOK)
	io.Copy(c.Writer, resp.Body)
	return nil
}

// UploadFile handles POST /sandboxes/:id/fs/upload?path=/path/to/file
func (h *FSHandler) UploadFile(c *gin.Context) error {
	id := c.Param("id")
	targetPath := c.Query("path")
	if targetPath == "" {
		return util.ErrBadRequest("path is required")
	}
	if err := validatePath(targetPath); err != nil {
		return util.ErrBadRequest("Invalid path", err.Error())
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var bodyReader io.Reader
	var contentLength string
	var contentType string

	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		const maxSize = 5 // 5 MB
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSize<<20)

		fileHeader, err := c.FormFile("file")
		if err != nil {
			if strings.Contains(err.Error(), "request body too large") {
				return util.ErrTooLarge(fmt.Sprintf("File too large. Maximum %dMB for multipart uploads. Use binary upload for larger files.", maxSize))
			}
			return util.ErrBadRequest("No file found in multipart upload. Expected field name 'file'", err.Error())
		}

		file, err := fileHeader.Open()
		if err != nil {
			return util.ErrInternal("Failed to open uploaded file", err)
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
		bodyReader = c.Request.Body
		contentLength = c.Request.Header.Get("Content-Length")
		contentType = c.Request.Header.Get("Content-Type")
		log.Printf("[FS] Binary upload: path: %s, size: %s, type: %s", targetPath, contentLength, contentType)
	}

	resp, err := h.fsService.UploadFile(c.Request.Context(), id, targetPath, bodyReader, contentLength, contentType)
	if err != nil {
		return util.ErrBadGateway("Upload failed", err)
	}
	defer resp.Body.Close()

	c.Status(resp.StatusCode)
	h.streamCopy(c.Writer, resp.Body)
	return nil
}

// DeleteFile handles DELETE /sandboxes/:id/fs?path=/path/to/file
func (h *FSHandler) DeleteFile(c *gin.Context) error {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		return util.ErrBadRequest("path is required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.DeleteFile(c.Request.Context(), id, filePath)
	if err != nil {
		return util.ErrBadGateway("Failed to delete file", err)
	}
	return HandleJSONResponse(c, resp)
}

// CreateDirectory handles POST /sandboxes/:id/fs/mkdir?path=/path/to/dir
func (h *FSHandler) CreateDirectory(c *gin.Context) error {
	id := c.Param("id")
	dirPath := c.Query("path")
	if dirPath == "" {
		return util.ErrBadRequest("path is required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.CreateDirectory(c.Request.Context(), id, dirPath)
	if err != nil {
		return util.ErrBadGateway("Failed to create directory", err)
	}
	return HandleJSONResponse(c, resp)
}

// MoveFile handles POST /sandboxes/:id/fs/move?from=/path/from&to=/path/to
func (h *FSHandler) MoveFile(c *gin.Context) error {
	id := c.Param("id")
	sourcePath := c.Query("from")
	destPath := c.Query("to")
	if sourcePath == "" || destPath == "" {
		return util.ErrBadRequest("from and to query params are required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.MoveFile(c.Request.Context(), id, sourcePath, destPath)
	if err != nil {
		return util.ErrBadGateway("Failed to move file", err)
	}
	return HandleJSONResponse(c, resp)
}

// CreateFile handles POST /sandboxes/:id/files/create?path=/path/to/file
func (h *FSHandler) CreateFile(c *gin.Context) error {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		return util.ErrBadRequest("path is required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.CreateFile(c.Request.Context(), id, filePath)
	if err != nil {
		return util.ErrBadGateway("Failed to create file", err)
	}
	return HandleJSONResponse(c, resp)
}

// StatFile handles GET /sandboxes/:id/fs/stat?path=/path/to/file
func (h *FSHandler) StatFile(c *gin.Context) error {
	id := c.Param("id")
	filePath := c.Query("path")
	if filePath == "" {
		return util.ErrBadRequest("path is required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.StatFile(c.Request.Context(), id, filePath)
	if err != nil {
		return util.ErrBadGateway("Failed to get file info", err)
	}
	return HandleJSONResponse(c, resp)
}

// CopyFile handles POST /sandboxes/:id/files/copy?from=...&to=...
func (h *FSHandler) CopyFile(c *gin.Context) error {
	id := c.Param("id")
	from := c.Query("from")
	to := c.Query("to")
	if from == "" || to == "" {
		return util.ErrBadRequest("from and to required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.CopyFile(c.Request.Context(), id, from, to)
	if err != nil {
		return util.ErrBadGateway("Failed to copy file", err)
	}
	return HandleJSONResponse(c, resp)
}

// HeadTail handles GET /sandboxes/:id/files/head-tail?path=...&lines=10&head=true
func (h *FSHandler) HeadTail(c *gin.Context) error {
	id := c.Param("id")
	path := c.Query("path")
	if path == "" {
		return util.ErrBadRequest("path required")
	}
	if err := validatePath(path); err != nil {
		return util.ErrBadRequest("Invalid path", err.Error())
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

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.HeadTail(c.Request.Context(), id, path, lines, isHead)
	if err != nil {
		return util.ErrBadGateway("Failed to read file", err)
	}
	return HandleJSONResponse(c, resp)
}

// ChangePermissions handles POST /sandboxes/:id/files/chmod?path=...&mode=755
func (h *FSHandler) ChangePermissions(c *gin.Context) error {
	id := c.Param("id")
	path := c.Query("path")
	mode := c.Query("mode")
	if path == "" || mode == "" {
		return util.ErrBadRequest("path and mode required")
	}
	if err := validatePath(path); err != nil {
		return util.ErrBadRequest("Invalid path", err.Error())
	}
	if len(mode) > maxModeLength {
		return util.ErrBadRequest("mode exceeds maximum length")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.ChangePermissions(c.Request.Context(), id, path, mode)
	if err != nil {
		return util.ErrBadGateway("Failed to change permissions", err)
	}
	return HandleJSONResponse(c, resp)
}

// DiskUsage handles GET /sandboxes/:id/files/du?path=...
func (h *FSHandler) DiskUsage(c *gin.Context) error {
	id := c.Param("id")
	path := c.Query("path")
	if path == "" {
		path = "/root"
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.DiskUsage(c.Request.Context(), id, path)
	if err != nil {
		return util.ErrBadGateway("Failed to get disk usage", err)
	}
	return HandleJSONResponse(c, resp)
}

// SearchFiles handles GET /sandboxes/:id/files/search?path=...&pattern=...
func (h *FSHandler) SearchFiles(c *gin.Context) error {
	id := c.Param("id")
	path := c.Query("path")
	pattern := c.Query("pattern")
	if path == "" {
		path = "/root"
	}
	if pattern == "" {
		return util.ErrBadRequest("pattern required")
	}
	if err := validatePath(path); err != nil {
		return util.ErrBadRequest("Invalid path", err.Error())
	}
	if len(pattern) > maxPatternLength {
		return util.ErrBadRequest("pattern exceeds maximum length")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.SearchFiles(c.Request.Context(), id, path, pattern)
	if err != nil {
		return util.ErrBadGateway("Failed to search files", err)
	}
	return HandleJSONResponse(c, resp)
}

// CompressFile handles POST /sandboxes/:id/files/compress?path=...&format=tar.gz
func (h *FSHandler) CompressFile(c *gin.Context) error {
	id := c.Param("id")
	path := c.Query("path")
	format := c.Query("format")
	if path == "" || format == "" {
		return util.ErrBadRequest("path and format required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.CompressFile(c.Request.Context(), id, path, format)
	if err != nil {
		return util.ErrBadGateway("Failed to compress file", err)
	}
	return HandleJSONResponse(c, resp)
}

// ExtractArchive handles POST /sandboxes/:id/files/extract?archive=...&dest=...
func (h *FSHandler) ExtractArchive(c *gin.Context) error {
	id := c.Param("id")
	archive := c.Query("archive")
	dest := c.Query("dest")
	if archive == "" {
		return util.ErrBadRequest("archive required")
	}
	if dest == "" {
		dest = filepath.Dir(archive)
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.fsService.ExtractArchive(c.Request.Context(), id, archive, dest)
	if err != nil {
		return util.ErrBadGateway("Failed to extract archive", err)
	}
	return HandleJSONResponse(c, resp)
}

// StartWatch handles POST /sandboxes/:id/files/watch/start
func (h *FSHandler) StartWatch(c *gin.Context) error {
	id := c.Param("id")

	var req struct {
		Path         string `json:"path" binding:"required"`
		Recursive    bool   `json:"recursive"`
		IgnoreHidden *bool  `json:"ignoreHidden"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request body", err.Error())
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	ignoreHidden := true
	if req.IgnoreHidden != nil {
		ignoreHidden = *req.IgnoreHidden
	}

	resp, err := h.fsService.StartWatch(c.Request.Context(), id, req.Path, req.Recursive, ignoreHidden)
	if err != nil {
		return util.ErrBadGateway("Failed to start watch", err)
	}
	defer resp.Body.Close()

	var agentResp struct {
		Success   bool   `json:"success"`
		Error     string `json:"error"`
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
		return util.ErrBadGateway("Invalid sandbox response", err)
	}
	if !agentResp.Success {
		return util.ErrBadGateway("Sandbox error: "+agentResp.Error, nil)
	}
	if agentResp.SessionID == "" {
		return util.ErrBadGateway("Sandbox error: missing sessionId", nil)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("watch started", map[string]interface{}{
		"sessionId": agentResp.SessionID,
	}))
	return nil
}

// StreamWatchEvents handles WebSocket streaming of file watch events.
// WebSocket safety: returns nil after upgrade — see Handle() doc comment.
func (h *FSHandler) StreamWatchEvents(c *gin.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	if sessionID == "" {
		return util.ErrBadRequest("sessionId is required")
	}
	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	clientConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[Watch] Failed to upgrade client connection: %v", err)
		return nil // upgrader already wrote error response
	}
	defer clientConn.Close()

	agentURL := fmt.Sprintf("ws://%s/watch/stream?sessionId=%s", id, sessionID)
	agentConn, _, err := h.dialer.DialContext(c.Request.Context(), agentURL, nil)
	if err != nil {
		log.Printf("[Watch] Failed to connect to agent: %v", err)
		clientConn.WriteJSON(map[string]string{"error": "Failed to connect to watch session"})
		return nil
	}
	defer agentConn.Close()

	log.Printf("[Watch] Streaming events from session %s to client", sessionID)

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

	// Client -> Agent (ping/pong to detect disconnect)
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
	return nil
}
