package handler

import (
	"net/http"

	"voidrun/service"

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
func (h *UserHandler) GetMe(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Get user by ID
	user, err := h.userService.GetByID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
		return
	}

	if user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// Get all orgs the user is a member of
	orgs, err := h.orgService.ListByMemberID(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user organizations"})
		return
	}

	// Convert orgs to OrgInfo
	orgInfos := make([]OrgInfo, len(orgs))
	for i, org := range orgs {
		orgInfos[i] = OrgInfo{
			ID:   org.ID.Hex(),
			Name: org.Name,
		}
	}

	response := MeResponse{
		ID:        user.ID.Hex(),
		Name:      user.Name,
		Email:     user.Email,
		ImageURL:  user.ImageURL,
		CreatedAt: user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Orgs:      orgInfos,
	}

	c.JSON(http.StatusOK, response)
}
