package handler

import (
	"net/http"
	"regexp"
	"strings"

	"voidrun/internal/model"
	"voidrun/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	maxNameLength  = 100
	maxEmailLength = 254
)

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

// AuthHandler handles registration and provisioning
type AuthHandler struct {
	users   *service.UserService
	orgs    *service.OrgService
	apiKeys *service.APIKeyService
}

func NewAuthHandler(users *service.UserService, orgs *service.OrgService, apiKeys *service.APIKeyService) *AuthHandler {
	return &AuthHandler{users: users, orgs: orgs, apiKeys: apiKeys}
}

// Register handles user registration
// @Router /register [post]
func (h *AuthHandler) Register(c *gin.Context) {
	var req model.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Validate and sanitize email
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Email is required", ""))
		return
	}
	if len(req.Email) > maxEmailLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Email exceeds maximum length", ""))
		return
	}
	if !emailRegex.MatchString(req.Email) {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid email format", ""))
		return
	}

	if req.Name == "" {
		// Derive name from email local part
		parts := strings.Split(req.Email, "@")
		if len(parts) > 0 {
			req.Name = parts[0]
		} else {
			req.Name = req.Email
		}
	}

	// Validate and sanitize name
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Name) > maxNameLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Name exceeds maximum length", ""))
		return
	}

	user, err := h.users.Register(c.Request.Context(), &req)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	org, err := h.orgs.EnsureDefaultOrg(c.Request.Context(), user.ID, user.Name+" Org")
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Generate an API key for the org owned by this user
	gen, err := h.apiKeys.GenerateKey(c.Request.Context(), org.ID, user.ID, "default")
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusCreated, model.NewSuccessResponse("Provisioned", gin.H{
		"user":   gin.H{"id": user.ID.Hex(), "name": user.Name, "email": user.Email},
		"org":    gin.H{"id": org.ID.Hex(), "name": org.Name},
		"apiKey": gen,
	}))
}
