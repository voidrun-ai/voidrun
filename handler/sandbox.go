package handler

import (
	"net/http"
	"strconv"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

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
	// Extract orgID from context (injected by auth middleware)
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
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

	sbxList, total, actualPageSize, err := h.sandboxService.ListByOrgPaginated(c.Request.Context(), orgID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to list sandboxes", err.Error()))
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
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	req.OrgID = orgID

	userID, err := util.GetUserIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}
	req.UserID = userID

	spec, err := h.sandboxService.Create(c.Request.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "Sandbox ID already exists in DB" {
			status = http.StatusConflict
		}
		c.JSON(status, model.NewErrorResponse("Failed to create sandbox", err.Error()))
		return
	}

	c.JSON(http.StatusCreated, model.NewSuccessResponse("Sandbox created", map[string]string{"id": spec.ID.Hex()}))
}

func (h *SandboxHandler) Get(c *gin.Context) {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	sandbox, err := h.sandboxService.Get(c.Request.Context(), orgID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to fetch", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox details fetched", sandbox))
}

func (h *SandboxHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if err := h.sandboxService.Delete(c.Request.Context(), orgID, id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Delete failed", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox deleted", nil))
}

func (h *SandboxHandler) Start(c *gin.Context) {
	id := c.Param("id")
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if err := h.sandboxService.Start(c.Request.Context(), orgID, id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Start failed", err.Error()))
		return
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox started", nil))
}

func (h *SandboxHandler) Stop(c *gin.Context) {
	id := c.Param("id")
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if err := h.sandboxService.Stop(c.Request.Context(), orgID, id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Stop failed", err.Error()))
		return
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox stopped", nil))
}

func (h *SandboxHandler) Pause(c *gin.Context) {
	id := c.Param("id")
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if err := h.sandboxService.Pause(c.Request.Context(), orgID, id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Pause failed", err.Error()))
		return
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox paused", nil))
}

func (h *SandboxHandler) Resume(c *gin.Context) {
	id := c.Param("id")
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if err := h.sandboxService.Resume(c.Request.Context(), orgID, id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Resume failed", err.Error()))
		return
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox resumed", nil))
}
