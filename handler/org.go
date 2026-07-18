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
		Balance:   org.Balance,
		CreatedAt: org.CreatedAt,
		UpdatedAt: org.UpdatedAt,
	}
}

// NewOrgHandler creates a new OrgHandler
func NewOrgHandler(orgSvc *service.OrgService, apiSvc *service.APIKeyService, userSvc *service.UserService) *OrgHandler {
	return &OrgHandler{apiKeyService: apiSvc, orgService: orgSvc, userService: userSvc}
}

// GetCurrentOrg returns org info for the authenticated user (GET /api/orgs/me)
func (h *OrgHandler) GetCurrentOrg(c *gin.Context) error {
	orgHex := c.GetString("orgID")
	log.Printf("[OrgHandler] GetCurrentOrg called with orgID: %s\n", orgHex)
	if orgHex == "" {
		return util.ErrUnauthorized("missing org context")
	}

	orgID, err := primitive.ObjectIDFromHex(orgHex)
	if err != nil {
		return util.ErrBadRequest("invalid org id", err.Error())
	}

	var userID primitive.ObjectID
	if userIDHex := c.GetString("userID"); userIDHex != "" {
		if parsedUserID, err := primitive.ObjectIDFromHex(userIDHex); err == nil {
			userID = parsedUserID
		}
	}

	allOrgs, err := h.orgService.GetCurrentOrg(c.Request.Context(), orgID, userID)
	if err != nil {
		return util.ErrInternal(err.Error(), nil)
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
	return nil
}

// GetOrgUsers returns users for an organization (GET /api/orgs/users)
func (h *OrgHandler) GetOrgUsers(c *gin.Context) error {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	org, err := h.orgService.GetByID(c.Request.Context(), orgID)
	if err != nil {
		return util.ErrInternal(err.Error(), nil)
	}
	if org == nil {
		return util.ErrNotFound("org not found")
	}

	users, err := h.userService.GetByOrg(c.Request.Context(), org.Members)
	if err != nil {
		return util.ErrInternal(err.Error(), nil)
	}

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
	return nil
}

// GenerateAPIKey creates a new API key for an org (POST /api/orgs/apikeys)
func (h *OrgHandler) GenerateAPIKey(c *gin.Context) error {
	var req struct {
		KeyName string `json:"keyName" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return util.ErrBadRequest(err.Error())
	}

	req.KeyName = strings.TrimSpace(req.KeyName)
	if err := util.ValidateAPIKeyName(req.KeyName); err != nil {
		return util.ErrBadRequest(err.Error())
	}

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	userID, err := util.GetUserIDFromContext(c)
	if err != nil {
		return err
	}

	resp, err := h.apiKeyService.GenerateKey(c.Request.Context(), orgID, userID, req.KeyName)
	if err != nil {
		return util.ErrInternal(err.Error(), nil)
	}

	c.JSON(http.StatusCreated, resp)
	return nil
}

// ListAPIKeys returns all API keys for an org (GET /api/orgs/apikeys)
func (h *OrgHandler) ListAPIKeys(c *gin.Context) error {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	keys, err := h.apiKeyService.ListByOrg(c.Request.Context(), orgID)
	if err != nil {
		return util.ErrInternal(err.Error(), nil)
	}

	c.JSON(http.StatusOK, keys)
	return nil
}

// DeleteAPIKey revokes an API key (DELETE /api/orgs/:orgId/apikeys/:keyId)
func (h *OrgHandler) DeleteAPIKey(c *gin.Context) error {
	keyID := c.Param("keyId")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err := h.apiKeyService.RevokeKeyByOrg(c.Request.Context(), orgID, keyID); err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			return util.ErrNotFound("API key not found")
		}
		return util.ErrInternal(err.Error(), nil)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("API key revoked", nil))
	return nil
}

// ActivateAPIKey toggles activation status (POST /api/orgs/:orgId/apikeys/:keyId/activate)
func (h *OrgHandler) ActivateAPIKey(c *gin.Context) error {
	keyID := c.Param("keyId")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	var req struct {
		IsActive bool `json:"isActive"`
	}
	if err = c.ShouldBindJSON(&req); err != nil {
		return util.ErrBadRequest(err.Error())
	}

	if req.IsActive {
		err = h.apiKeyService.ActivateKeyByOrg(c.Request.Context(), orgID, keyID)
	} else {
		err = h.apiKeyService.DeactivateKeyByOrg(c.Request.Context(), orgID, keyID)
	}
	if err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			return util.ErrNotFound("API key not found")
		}
		return util.ErrInternal(err.Error(), nil)
	}

	msg := "API key deactivated"
	if req.IsActive {
		msg = "API key activated"
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse(msg, nil))
	return nil
}

// TouchAPIKey marks a key as used (PATCH /api/orgs/:orgId/apikeys/:keyId/touch)
func (h *OrgHandler) TouchAPIKey(c *gin.Context) error {
	keyID := c.Param("keyId")
	orgID := c.GetString("orgID")

	if err := validateObjectID(keyID); err != nil {
		return util.ErrBadRequest("Invalid key ID format", err.Error())
	}

	if err := h.apiKeyService.TouchKeyByOrg(c.Request.Context(), orgID, keyID, time.Now()); err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			return util.ErrNotFound("API key not found")
		}
		return util.ErrInternal(err.Error(), nil)
	}
	c.JSON(http.StatusOK, model.NewSuccessResponse("API key touched", nil))
	return nil
}
