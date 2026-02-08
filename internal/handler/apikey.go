package handler

import (
	"net/http"
	"time"

	"voidrun/internal/model"
	"voidrun/internal/service"

	"github.com/gin-gonic/gin"
)

// APIKeyHandler handles organization API key endpoints
type APIKeyHandler struct {
	service *service.APIKeyService
}

// NewAPIKeyHandler creates a new APIKeyHandler
func NewAPIKeyHandler(svc *service.APIKeyService) *APIKeyHandler {
	return &APIKeyHandler{service: svc}
}

// Generate creates a new API key for an org (POST /api/orgs/:orgId/apikeys)
func (h *APIKeyHandler) Generate(c *gin.Context) {
	orgID := c.Param("orgId")

	var req struct {
		KeyName string `json:"keyName" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Optional: get userID from context if set by auth middleware
	var userIDHex string
	if v, ok := c.Get("userID"); ok {
		if s, ok2 := v.(string); ok2 {
			userIDHex = s
		}
	}

	resp, err := h.service.GenerateKeyFromStrings(c.Request.Context(), orgID, userIDHex, req.KeyName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// List returns all API keys for an org (GET /api/orgs/:orgId/apikeys)
func (h *APIKeyHandler) List(c *gin.Context) {
	orgID := c.Param("orgId")

	keys, err := h.service.ListByOrgID(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, keys)
}

// Delete revokes an API key (DELETE /api/orgs/:orgId/apikeys/:keyId)
func (h *APIKeyHandler) Delete(c *gin.Context) {
	keyID := c.Param("keyId")

	if err := h.service.RevokeKey(c.Request.Context(), keyID); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("API key revoked", nil))
}

// Activate toggles activation status (POST /api/orgs/:orgId/apikeys/:keyId/activate)
func (h *APIKeyHandler) Activate(c *gin.Context) {
	keyID := c.Param("keyId")

	var req struct {
		IsActive bool `json:"isActive"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	var err error
	if req.IsActive {
		err = h.service.ActivateKey(c.Request.Context(), keyID)
	} else {
		err = h.service.DeactivateKey(c.Request.Context(), keyID)
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	msg := "API key deactivated"
	if req.IsActive {
		msg = "API key activated"
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse(msg, nil))
}

// Touch marks a key as used (optional endpoint) PATCH /api/orgs/:orgId/apikeys/:keyId/touch
func (h *APIKeyHandler) Touch(c *gin.Context) {
	keyID := c.Param("keyId")
	if err := h.service.TouchKey(c.Request.Context(), keyID, time.Now()); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("API key touched", nil))
}
