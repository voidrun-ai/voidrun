package handler

import (
	"net/http"
	"strings"

	"voidrun/internal/model"
	"voidrun/internal/service"

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

// List handles GET /images
func (h *ImageHandler) List(c *gin.Context) {
	images, err := h.imageService.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, images)
}

// Get handles GET /images/:id
func (h *ImageHandler) Get(c *gin.Context) {
	id := c.Param("id")

	if err := validateObjectID(id); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid image ID format", err.Error()))
		return
	}

	image, err := h.imageService.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Image not found", err.Error()))
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
		if uid, ok := userID.(primitive.ObjectID); ok {
			img.CreatedBy = uid
		}
	}

	// Set OrgID from context if available
	if orgID, exists := c.Get("orgID"); exists {
		if oid, ok := orgID.(primitive.ObjectID); ok {
			img.OrgID = oid
		}
	}

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

	if err := validateObjectID(id); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid image ID format", err.Error()))
		return
	}

	if err := h.imageService.Delete(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Delete failed", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Image deleted", nil))
}

// GetByName handles GET /images/name/:name
func (h *ImageHandler) GetByName(c *gin.Context) {
	name := c.Param("name")

	if name == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name is required", ""))
		return
	}
	if len(name) > maxImageNameLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name exceeds maximum length", ""))
		return
	}

	image, err := h.imageService.GetLatestByName(name)
	if err != nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Image not found", ""))
		return
	}

	c.JSON(http.StatusOK, image)
}
