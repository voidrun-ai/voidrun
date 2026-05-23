package handler

import (
	"net/http"
	"strconv"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
)

type SandboxHandler struct {
	sandboxService *service.SandboxService
}

func NewSandboxHandler(sandboxService *service.SandboxService) *SandboxHandler {
	return &SandboxHandler{sandboxService: sandboxService}
}

// List handles GET /sandboxes with pagination
func (h *SandboxHandler) List(c *gin.Context) error {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	page := 1
	pageSize := 0

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
		return util.ErrInternal("Failed to list sandboxes", err)
	}
	if sbxList == nil {
		sbxList = []*model.Sandbox{}
	}

	totalPages := (total + int64(actualPageSize) - 1) / int64(actualPageSize)

	c.JSON(http.StatusOK, model.NewSuccessResponseWithMeta("Sandboxes fetched", sbxList, map[string]interface{}{
		"page":       page,
		"limit":      actualPageSize,
		"total":      total,
		"totalPages": totalPages,
	}))
	return nil
}

func (h *SandboxHandler) Create(c *gin.Context) error {
	var req model.CreateSandboxRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return util.ErrBadRequest(err.Error())
	}

	if err := util.ValidateCreateSandboxRequest(req.Name, req.CPU, req.Mem); err != nil {
		return util.ErrBadRequest(err.Error())
	}

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}
	req.OrgID = orgID

	userID, err := util.GetUserIDFromContext(c)
	if err != nil {
		return err
	}
	req.UserID = userID

	spec, err := h.sandboxService.Create(c.Request.Context(), req)
	if err != nil {
		if err.Error() == "Sandbox ID already exists in DB" {
			return util.ErrConflict("Sandbox ID already exists")
		}
		return util.ErrInternal("Failed to create sandbox", err)
	}

	c.JSON(http.StatusCreated, model.NewSuccessResponse("Sandbox created", spec))
	return nil
}

func (h *SandboxHandler) Get(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	sandbox, err := h.sandboxService.Get(c.Request.Context(), orgID, id)
	if err != nil {
		return util.ErrInternal("Failed to fetch", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox details fetched", sandbox))
	return nil
}

func (h *SandboxHandler) Delete(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err := h.sandboxService.Delete(c.Request.Context(), orgID, id); err != nil {
		return util.ErrInternal("Delete failed", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox deleted", nil))
	return nil
}

func (h *SandboxHandler) Start(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err := h.sandboxService.Start(c.Request.Context(), orgID, id); err != nil {
		return util.ErrInternal("Start failed", err)
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox started", nil))
	return nil
}

func (h *SandboxHandler) Stop(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err := h.sandboxService.Stop(c.Request.Context(), orgID, id); err != nil {
		return util.ErrInternal("Stop failed", err)
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox stopped", nil))
	return nil
}

func (h *SandboxHandler) Pause(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err := h.sandboxService.Pause(c.Request.Context(), orgID, id); err != nil {
		return util.ErrInternal("Pause failed", err)
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox paused", nil))
	return nil
}

func (h *SandboxHandler) Resume(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err := h.sandboxService.Resume(c.Request.Context(), orgID, id); err != nil {
		return util.ErrInternal("Resume failed", err)
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("Sandbox resumed", nil))
	return nil
}
