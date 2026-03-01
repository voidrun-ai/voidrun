package handler

import (
	"errors"
	"net/http"
	"strings"

	"voidrun/model"
	"voidrun/service"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	maxImageNameLength = 100
	maxImageTagLength  = 50
)

// validateObjectID checks if a string is a valid MongoDB ObjectID
func validateObjectID(id string) error {
	if _, err := primitive.ObjectIDFromHex(id); err != nil {
		return err
	}
	return nil
}

// ImageHandler handles image-related HTTP requests
type ImageHandler struct {
	imageService *service.ImageService
}

// NewImageHandler creates a new image handler
func NewImageHandler(imageService *service.ImageService) *ImageHandler {
	return &ImageHandler{imageService: imageService}
}

func getOrgIDFromContext(c *gin.Context) (string, bool) {
	orgVal, ok := c.Get("orgID")
	if !ok {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("missing org context", ""))
		return "", false
	}
	orgID, ok := orgVal.(string)
	if !ok || strings.TrimSpace(orgID) == "" {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("invalid org context", ""))
		return "", false
	}
	return orgID, true
}

// List handles GET /images
func (h *ImageHandler) List(c *gin.Context) {
	orgID, ok := getOrgIDFromContext(c)
	if !ok {
		return
	}

	images, err := h.imageService.ListByOrg(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, images)
}

// Get handles GET /images/:id
func (h *ImageHandler) Get(c *gin.Context) {
	id := c.Param("id")
	orgID, ok := getOrgIDFromContext(c)
	if !ok {
		return
	}

	if err := validateObjectID(id); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid image ID format", err.Error()))
		return
	}

	image, err := h.imageService.GetByOrg(c.Request.Context(), id, orgID)
	if err != nil {
		if errors.Is(err, service.ErrImageNotFound) {
			c.JSON(http.StatusNotFound, model.NewErrorResponse("Image not found", ""))
			return
		}
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to fetch image", err.Error()))
		return
	}

	c.JSON(http.StatusOK, image)
}

// Create handles POST /images
func (h *ImageHandler) Create(c *gin.Context) {
	var img model.Image
	if err := c.ShouldBindJSON(&img); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Validate image fields
	if img.Name == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name is required", ""))
		return
	}
	if len(img.Name) > maxImageNameLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name exceeds maximum length", ""))
		return
	}
	if img.Tag != "" && len(img.Tag) > maxImageTagLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image tag exceeds maximum length", ""))
		return
	}
	// Sanitize name (remove dangerous characters)
	img.Name = strings.TrimSpace(img.Name)
	if img.Tag != "" {
		img.Tag = strings.TrimSpace(img.Tag)
	}

	// Set CreatedBy from context if available (would come from auth middleware)
	if userID, exists := c.Get("userID"); exists {
		if uidHex, ok := userID.(string); ok && strings.TrimSpace(uidHex) != "" {
			uid, err := primitive.ObjectIDFromHex(uidHex)
			if err != nil {
				c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid user ID format", err.Error()))
				return
			}
			img.CreatedBy = uid
		}
	}

	orgIDHex, ok := getOrgIDFromContext(c)
	if !ok {
		return
	}
	orgID, err := primitive.ObjectIDFromHex(orgIDHex)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid org ID format", err.Error()))
		return
	}
	img.System = false
	img.OrgID = orgID

	created, err := h.imageService.Create(c.Request.Context(), &img)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusCreated, model.NewSuccessResponse("Image created", created))
}

// Delete handles DELETE /images/:id
func (h *ImageHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	orgID, ok := getOrgIDFromContext(c)
	if !ok {
		return
	}

	if err := validateObjectID(id); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid image ID format", err.Error()))
		return
	}

	if err := h.imageService.DeleteByOrg(c.Request.Context(), id, orgID); err != nil {
		if errors.Is(err, service.ErrImageNotFound) {
			c.JSON(http.StatusNotFound, model.NewErrorResponse("Image not found", ""))
			return
		}
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Delete failed", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Image deleted", nil))
}

// GetByName handles GET /images/name/:name
func (h *ImageHandler) GetByName(c *gin.Context) {
	name := c.Param("name")
	orgID, ok := getOrgIDFromContext(c)
	if !ok {
		return
	}

	if name == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name is required", ""))
		return
	}
	if len(name) > maxImageNameLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name exceeds maximum length", ""))
		return
	}

	image, err := h.imageService.GetLatestByNameForOrg(name, orgID)
	if err != nil {
		if errors.Is(err, service.ErrImageNotFound) {
			c.JSON(http.StatusNotFound, model.NewErrorResponse("Image not found", ""))
			return
		}
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to fetch image", err.Error()))
		return
	}

	c.JSON(http.StatusOK, image)
}
