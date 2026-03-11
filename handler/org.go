package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const maxKeyNameLength = 100

// OrgHandler handles organization-scoped endpoints (including API keys)
type OrgHandler struct {
	apiKeyService *service.APIKeyService
	orgService    *service.OrgService
	userService   *service.UserService
}

func orgToResponse(org *model.Org) model.OrgResponse {
	return model.OrgResponse{
		ID:        org.ID.Hex(),
		Name:      org.Name,
		Plan:      org.Plan,
		Usage:     org.UsageCount,
		CreatedAt: org.CreatedAt,
		UpdatedAt: org.UpdatedAt,
	}
}

// NewOrgHandler creates a new OrgHandler
func NewOrgHandler(orgSvc *service.OrgService, apiSvc *service.APIKeyService, userSvc *service.UserService) *OrgHandler {
	return &OrgHandler{apiKeyService: apiSvc, orgService: orgSvc, userService: userSvc}
}

// GetCurrentOrg returns org info for the authenticated API key (GET /api/orgs/me)
func (h *OrgHandler) GetCurrentOrg(c *gin.Context) {
	orgHex := c.GetString("orgID")
	log.Printf("[OrgHandler] GetCurrentOrg called with orgID: %s\n", orgHex)
	if orgHex == "" {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("missing org context", ""))
		return
	}

	orgID, err := primitive.ObjectIDFromHex(orgHex)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("invalid org id", err.Error()))
		return
	}

	var userID primitive.ObjectID
	if userIDHex := c.GetString("userID"); userIDHex != "" {
		if parsedUserID, err := primitive.ObjectIDFromHex(userIDHex); err == nil {
			userID = parsedUserID
		}
	}

	allOrgs, err := h.orgService.GetCurrentOrg(c.Request.Context(), orgID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	orgList := make([]model.OrgResponse, len(allOrgs))
	for i, o := range allOrgs {
		orgList[i] = orgToResponse(o)
	}

	resp := model.CurrentOrgResponse{
		ActiveOrgID: orgID.Hex(),
		Orgs:        orgList,
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("org", resp))
}

// GetOrgUsers returns users for an organization (GET /api/orgs/users)
func (h *OrgHandler) GetOrgUsers(c *gin.Context) {

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	org, err := h.orgService.GetByID(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
	if org == nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("org not found", ""))
		return
	}

	// Get users by member IDs
	users, err := h.userService.GetByOrg(c.Request.Context(), org.Members)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Transform users to safe public format
	publicUsers := make([]gin.H, len(users))
	for i, u := range users {
		publicUsers[i] = gin.H{
			"id":        u.ID.Hex(),
			"name":      u.Name,
			"email":     u.Email,
			"imageUrl":  u.ImageURL,
			"createdAt": u.CreatedAt,
		}
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("users", publicUsers))
}

// GenerateAPIKey creates a new API key for an org (POST /api/orgs/apikeys)
func (h *OrgHandler) GenerateAPIKey(c *gin.Context) {
	var req struct {
		KeyName string `json:"keyName" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Validate key name
	req.KeyName = strings.TrimSpace(req.KeyName)
	if req.KeyName == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Key name cannot be empty", ""))
		return
	}
	if len(req.KeyName) > maxKeyNameLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Key name exceeds maximum length", ""))
		return
	}

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	userID, err := util.GetUserIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	resp, err := h.apiKeyService.GenerateKey(c.Request.Context(), orgID, userID, req.KeyName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// ListAPIKeys returns all API keys for an org (GET /api/orgs/apikeys)
func (h *OrgHandler) ListAPIKeys(c *gin.Context) {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	keys, err := h.apiKeyService.ListByOrg(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, keys)
}

// DeleteAPIKey revokes an API key (DELETE /api/orgs/:orgId/apikeys/:keyId)
func (h *OrgHandler) DeleteAPIKey(c *gin.Context) {
	keyID := c.Param("keyId")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if err := h.apiKeyService.RevokeKeyByOrg(c.Request.Context(), orgID, keyID); err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			c.JSON(http.StatusNotFound, model.NewErrorResponse("API key not found", ""))
			return
		}
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("API key revoked", nil))
}

// ActivateAPIKey toggles activation status (POST /api/orgs/:orgId/apikeys/:keyId/activate)
func (h *OrgHandler) ActivateAPIKey(c *gin.Context) {
	keyID := c.Param("keyId")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	var req struct {
		IsActive bool `json:"isActive"`
	}
	if err = c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if req.IsActive {
		err = h.apiKeyService.ActivateKeyByOrg(c.Request.Context(), orgID, keyID)
	} else {
		err = h.apiKeyService.DeactivateKeyByOrg(c.Request.Context(), orgID, keyID)
	}

	if err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			c.JSON(http.StatusNotFound, model.NewErrorResponse("API key not found", ""))
			return
		}
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	msg := "API key deactivated"
	if req.IsActive {
		msg = "API key activated"
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse(msg, nil))
}

// TouchAPIKey marks a key as used (PATCH /api/orgs/:orgId/apikeys/:keyId/touch)
func (h *OrgHandler) TouchAPIKey(c *gin.Context) {
	keyID := c.Param("keyId")
	orgID := c.GetString("orgID")

	if err := validateObjectID(keyID); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid key ID format", err.Error()))
		return
	}

	if err := h.apiKeyService.TouchKeyByOrg(c.Request.Context(), orgID, keyID, time.Now()); err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			c.JSON(http.StatusNotFound, model.NewErrorResponse("API key not found", ""))
			return
		}
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("API key touched", nil))
}
