package handler

import (
	"net/http"
	"path/filepath"
	"strconv"

	"voidrun/internal/model"
	"voidrun/internal/service"
	"voidrun/pkg/util"

	"github.com/gin-gonic/gin"
)

const (
	minCPU    = 1
	maxCPU    = 8     // Max 8 vCPUs per sandbox
	minMemMiB = 1024  // Min 1 GiB
	maxMemMiB = 16384 // Max 16 GiB per sandbox
)

type SandboxHandler struct {
	sandboxService *service.SandboxService
}

func NewSandboxHandler(sandboxService *service.SandboxService) *SandboxHandler {
	return &SandboxHandler{sandboxService: sandboxService}
}

// List handles GET /sandboxes with pagination
func (h *SandboxHandler) List(c *gin.Context) {
	// Get orgID from auth context
	orgIDHex, ok := c.Get("orgID")
	if !ok {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("missing org context", ""))
		return
	}

	// Parse pagination params - will be validated by service
	page := 1
	pageSize := 0 // Let service use default from config

	if p := c.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if s := c.Query("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			pageSize = v
		}
	}

	sbxList, total, actualPageSize, err := h.sandboxService.ListByOrgPaginated(c.Request.Context(), orgIDHex.(string), page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
	if sbxList == nil {
		sbxList = []*model.Sandbox{}
	}

	// Calculate total pages for convenience
	totalPages := (total + int64(actualPageSize) - 1) / int64(actualPageSize)

	c.JSON(http.StatusOK, model.NewSuccessResponseWithMeta("Sandboxes fetched", sbxList, map[string]interface{}{
		"page":       page,
		"limit":      actualPageSize,
		"total":      total,
		"totalPages": totalPages,
	}))
}

func (h *SandboxHandler) Create(c *gin.Context) {
	var req model.CreateSandboxRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Validate sandbox name using DNS-1123 subdomain format
	if err := util.ValidateDNS1123Subdomain(req.Name); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("invalid name: "+err.Error(), ""))
		return
	}

	// Validate CPU count
	if req.CPU < minCPU || req.CPU > maxCPU {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(
			"invalid cpu count: must be between 1 and 8",
			"",
		))
		return
	}

	// Validate Memory (MiB)
	if req.Mem < minMemMiB || req.Mem > maxMemMiB {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(
			"invalid memory size: must be between 1 GiB and 16 GiB",
			"",
		))
		return
	}

	// Extract orgID and userID from context (injected by auth middleware)
	orgIDVal, ok := c.Get("orgID")
	if !ok {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("missing org context", ""))
		return
	}
	req.OrgID = orgIDVal.(string)

	// userIDVal, ok := c.Get("userID")
	// if ok {
	// 	req.UserID = userIDVal.(string)
	// }

	spec, err := h.sandboxService.Create(c.Request.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "Sandbox ID already exists in DB" {
			status = http.StatusConflict
		}
		c.JSON(status, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusCreated, model.NewSuccessResponse("Sandbox created", spec))
}

func (h *SandboxHandler) Restore(c *gin.Context) {
	var req model.RestoreSandboxRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Extract orgID and userID from context
	orgIDVal, ok := c.Get("orgID")
	if ok {
		req.OrgID = orgIDVal.(string)
	}
	userIDVal, ok := c.Get("userID")
	if ok {
		req.UserID = userIDVal.(string)
	}

	// Validate CPU count
	if req.CPU < minCPU || req.CPU > maxCPU {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(
			"invalid cpu count: must be between 1 and 16",
			"",
		))
		return
	}

	// Validate Memory (MiB)
	if req.Mem < minMemMiB || req.Mem > maxMemMiB {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(
			"invalid memory size: must be between 1 GiB and 16 GiB",
			"",
		))
		return
	}

	ip, err := h.sandboxService.Restore(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusCreated, model.NewSuccessResponse("Sandbox restored", gin.H{"ip": ip}))
}

func (h *SandboxHandler) Get(c *gin.Context) {
	id := c.Param("id")

	sandbox, ok := h.sandboxService.Get(c.Request.Context(), id)
	if !ok || sandbox == nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox details fetched", sandbox))
}

func (h *SandboxHandler) Delete(c *gin.Context) {
	id := c.Param("id")

	if err := h.sandboxService.Delete(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Delete failed", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox deleted", nil))
}

func (h *SandboxHandler) Stop(c *gin.Context) {
	h.sandboxAction(c, "stop", h.sandboxService.Stop)
}

func (h *SandboxHandler) Pause(c *gin.Context) {
	h.sandboxAction(c, "pause", h.sandboxService.Pause)
}

func (h *SandboxHandler) Resume(c *gin.Context) {
	h.sandboxAction(c, "resume", h.sandboxService.Resume)
}

func (h *SandboxHandler) Snapshot(c *gin.Context) {
	id := c.Param("id")

	if err := h.sandboxService.CreateSnapshot(id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Snapshot failed", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Snapshot created", gin.H{
		"base_path": h.sandboxService.GetSnapshotsBasePath(id),
	}))
}

func (h *SandboxHandler) ListSnapshots(c *gin.Context) {
	id := c.Param("id")

	snaps, err := h.sandboxService.ListSnapshots(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to scan snapshots", ""))
		return
	}

	c.JSON(http.StatusOK, snaps)
}

func (h *SandboxHandler) Upload(c *gin.Context) {
	id := c.Param("id")

	// Get target path from form data
	targetPath := c.PostForm("targetPath")
	if targetPath == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("targetPath is required", ""))
		return
	}

	// Get the uploaded file(s)
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Failed to parse multipart form", err.Error()))
		return
	}

	files := form.File["files"]
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("No files provided", ""))
		return
	}

	// Process upload
	uploadedFiles := []string{}
	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to open file", err.Error()))
			return
		}
		defer file.Close()

		err = h.sandboxService.UploadFile(c.Request.Context(), id, fileHeader.Filename, targetPath, fileHeader.Size, file)
		if err != nil {
			c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Upload failed", err.Error()))
			return
		}
		uploadedFiles = append(uploadedFiles, fileHeader.Filename)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Files uploaded successfully", gin.H{
		"uploaded_files": uploadedFiles,
		"target_path":    targetPath,
	}))
}

func (h *SandboxHandler) sandboxAction(c *gin.Context, action string, fn func(string) error) {
	id := c.Param("id")

	if err := fn(id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(action+" failed", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox "+action+"d", nil))
}

func GetSnapshotsBasePath(id string) string {
	pwd, _ := filepath.Abs(".")
	return filepath.Join(pwd, "instances", id, "snapshots")
}
