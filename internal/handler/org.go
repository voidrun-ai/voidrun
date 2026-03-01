package handler

import (
	"net/http"
	"strings"
	"time"

	"voidrun/internal/model"
	"voidrun/internal/service"

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

func orgToResponse(org *model.Organization) model.OrgResponse {
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

// ensureOrgAccess checks that the path orgId matches the org in auth context
func ensureOrgAccess(c *gin.Context) bool {
	pathOrg := c.Param("orgId")
	if val, ok := c.Get("orgID"); ok {
		if ctxOrg, ok2 := val.(string); ok2 {
			if ctxOrg == pathOrg {
				return true
			}
		}
	}
	c.JSON(http.StatusForbidden, model.NewErrorResponse("org mismatch or missing auth", ""))
	return false
}

// GetCurrentOrg returns org info for the authenticated API key (GET /api/orgs/me)
func (h *OrgHandler) GetCurrentOrg(c *gin.Context) {
	orgHex, ok := c.Get("orgID")
	if !ok {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("missing org context", ""))
		return
	}

	orgID, err := primitive.ObjectIDFromHex(orgHex.(string))
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("invalid org id", err.Error()))
		return
	}

	var userID *primitive.ObjectID
	if userHex, ok := c.Get("userID"); ok {
		if userIDStr, ok := userHex.(string); ok && strings.TrimSpace(userIDStr) != "" {
			if parsedUserID, err := primitive.ObjectIDFromHex(userIDStr); err == nil {
				userID = &parsedUserID
			}
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

// GetOrgUsers returns users for an organization (GET /api/orgs/:orgId/users)
func (h *OrgHandler) GetOrgUsers(c *gin.Context) {
	// Check org access using path param
	if !ensureOrgAccess(c) {
		return
	}

	orgID := c.Param("orgId")
	if err := validateObjectID(orgID); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid org ID format", err.Error()))
		return
	}

	objID, err := primitive.ObjectIDFromHex(orgID)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("invalid org id", err.Error()))
		return
	}

	org, err := h.orgService.GetByID(c.Request.Context(), objID)
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
			"role":      u.Role,
			"createdAt": u.CreatedAt,
		}
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("users", publicUsers))
}

// GenerateAPIKey creates a new API key for an org (POST /api/orgs/:orgId/apikeys)
func (h *OrgHandler) GenerateAPIKey(c *gin.Context) {
	if !ensureOrgAccess(c) {
		return
	}
	orgID := c.Param("orgId")

	if err := validateObjectID(orgID); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid org ID format", err.Error()))
		return
	}

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

	var userIDHex string
	if v, ok := c.Get("userID"); ok {
		if s, ok2 := v.(string); ok2 {
			userIDHex = s
		}
	}

	resp, err := h.apiKeyService.GenerateKeyFromStrings(c.Request.Context(), orgID, userIDHex, req.KeyName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// ListAPIKeys returns all API keys for an org (GET /api/orgs/:orgId/apikeys)
func (h *OrgHandler) ListAPIKeys(c *gin.Context) {
	if !ensureOrgAccess(c) {
		return
	}
	orgID := c.Param("orgId")

	if err := validateObjectID(orgID); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid org ID format", err.Error()))
		return
	}

	keys, err := h.apiKeyService.ListByOrgID(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, keys)
}

// DeleteAPIKey revokes an API key (DELETE /api/orgs/:orgId/apikeys/:keyId)
func (h *OrgHandler) DeleteAPIKey(c *gin.Context) {
	if !ensureOrgAccess(c) {
		return
	}
	keyID := c.Param("keyId")

	if err := validateObjectID(keyID); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid key ID format", err.Error()))
		return
	}

	if err := h.apiKeyService.RevokeKey(c.Request.Context(), keyID); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("API key revoked", nil))
}

// ActivateAPIKey toggles activation status (POST /api/orgs/:orgId/apikeys/:keyId/activate)
func (h *OrgHandler) ActivateAPIKey(c *gin.Context) {
	if !ensureOrgAccess(c) {
		return
	}
	keyID := c.Param("keyId")

	if err := validateObjectID(keyID); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid key ID format", err.Error()))
		return
	}

	var req struct {
		IsActive bool `json:"isActive"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	var err error
	if req.IsActive {
		err = h.apiKeyService.ActivateKey(c.Request.Context(), keyID)
	} else {
		err = h.apiKeyService.DeactivateKey(c.Request.Context(), keyID)
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

// TouchAPIKey marks a key as used (PATCH /api/orgs/:orgId/apikeys/:keyId/touch)
func (h *OrgHandler) TouchAPIKey(c *gin.Context) {
	if !ensureOrgAccess(c) {
		return
	}
	keyID := c.Param("keyId")

	if err := validateObjectID(keyID); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid key ID format", err.Error()))
		return
	}

	if err := h.apiKeyService.TouchKey(c.Request.Context(), keyID, time.Now()); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("API key touched", nil))
}
