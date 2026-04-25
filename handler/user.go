package handler

import (
	"net/http"

	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
)

type UserHandler struct {
	userService *service.UserService
	orgService  *service.OrgService
}

// MeResponse represents the response for /me endpoint
type MeResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	ImageURL  string    `json:"imageUrl"`
	CreatedAt string    `json:"createdAt"`
	Orgs      []OrgInfo `json:"orgs"`
}

// OrgInfo represents basic org information
type OrgInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NewUserHandler creates a new User handler
func NewUserHandler(userService *service.UserService, orgService *service.OrgService) *UserHandler {
	return &UserHandler{userService: userService, orgService: orgService}
}

// GetMe returns the current user's details and their organizations
func (h *UserHandler) GetMe(c *gin.Context) error {
	userID := c.GetString("userID")
	if userID == "" {
		return util.ErrUnauthorized("unauthorized")
	}

	user, err := h.userService.GetByID(c.Request.Context(), userID)
	if err != nil {
		return util.ErrInternal("failed to get user", err)
	}
	if user == nil {
		return util.ErrNotFound("user not found")
	}

	orgs, err := h.orgService.ListByMemberID(c.Request.Context(), user.ID)
	if err != nil {
		return util.ErrInternal("failed to get user organizations", err)
	}

	orgInfos := make([]OrgInfo, len(orgs))
	for i, org := range orgs {
		orgInfos[i] = OrgInfo{
			ID:   org.ID.Hex(),
			Name: org.Name,
		}
	}

	c.JSON(http.StatusOK, MeResponse{
		ID:        user.ID.Hex(),
		Name:      user.Name,
		Email:     user.Email,
		ImageURL:  user.ImageURL,
		CreatedAt: user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Orgs:      orgInfos,
	})
	return nil
}
