package handler

import (
	"errors"
	"net/http"
	"strings"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

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
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	images, err := h.imageService.ListByOrg(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	resp := make([]model.ImageResponse, len(images))
	for i, img := range images {
		resp[i] = model.NewImageResponse(img)
	}
	c.JSON(http.StatusOK, resp)
}

// Get handles GET /images/:id
func (h *ImageHandler) Get(c *gin.Context) {
	id := c.Param("id")
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	image, err := h.imageService.GetByOrg(c.Request.Context(), orgID, id)
	if err != nil {
		if errors.Is(err, service.ErrImageNotFound) {
			c.JSON(http.StatusNotFound, model.NewErrorResponse("Image not found", ""))
			return
		}
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to fetch image", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewImageResponse(image))
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

	img.CreatedBy = userID
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
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	if err := h.imageService.DeleteByOrg(c.Request.Context(), orgID, id); err != nil {
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

	if name == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name is required", ""))
		return
	}
	if len(name) > maxImageNameLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Image name exceeds maximum length", ""))
		return
	}

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
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

	c.JSON(http.StatusOK, model.NewImageResponse(image))
}
