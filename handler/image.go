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
func (h *ImageHandler) List(c *gin.Context) error {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	images, err := h.imageService.ListByOrg(c.Request.Context(), orgID)
	if err != nil {
		return util.ErrInternal(err.Error(), nil)
	}

	resp := make([]model.ImageResponse, len(images))
	for i, img := range images {
		resp[i] = model.NewImageResponse(img)
	}
	c.JSON(http.StatusOK, resp)
	return nil
}

// Get handles GET /images/:id
func (h *ImageHandler) Get(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	image, err := h.imageService.GetByOrg(c.Request.Context(), orgID, id)
	if err != nil {
		if errors.Is(err, service.ErrImageNotFound) {
			return util.ErrNotFound("Image not found")
		}
		return util.ErrInternal("Failed to fetch image", err)
	}

	c.JSON(http.StatusOK, model.NewImageResponse(image))
	return nil
}

// Create handles POST /images
func (h *ImageHandler) Create(c *gin.Context) error {
	var img model.Image
	if err := c.ShouldBindJSON(&img); err != nil {
		return util.ErrBadRequest(err.Error())
	}

	if img.Name == "" {
		return util.ErrBadRequest("Image name is required")
	}
	if len(img.Name) > maxImageNameLength {
		return util.ErrBadRequest("Image name exceeds maximum length")
	}
	if img.Tag != "" && len(img.Tag) > maxImageTagLength {
		return util.ErrBadRequest("Image tag exceeds maximum length")
	}

	img.Name = strings.TrimSpace(img.Name)
	if img.Tag != "" {
		img.Tag = strings.TrimSpace(img.Tag)
	}

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	userID, err := util.GetUserIDFromContext(c)
	if err != nil {
		return err
	}

	img.CreatedBy = userID
	img.System = false
	img.OrgID = orgID

	created, err := h.imageService.Create(c.Request.Context(), &img)
	if err != nil {
		return util.ErrInternal(err.Error(), nil)
	}

	c.JSON(http.StatusCreated, model.NewSuccessResponse("Image created", created))
	return nil
}

// Delete handles DELETE /images/:id
func (h *ImageHandler) Delete(c *gin.Context) error {
	id := c.Param("id")

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err := h.imageService.DeleteByOrg(c.Request.Context(), orgID, id); err != nil {
		if errors.Is(err, service.ErrImageNotFound) {
			return util.ErrNotFound("Image not found")
		}
		return util.ErrInternal("Delete failed", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Image deleted", nil))
	return nil
}

// GetByName handles GET /images/name/:name
func (h *ImageHandler) GetByName(c *gin.Context) error {
	name := c.Param("name")

	if name == "" {
		return util.ErrBadRequest("Image name is required")
	}
	if len(name) > maxImageNameLength {
		return util.ErrBadRequest("Image name exceeds maximum length")
	}

	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	image, err := h.imageService.GetLatestByNameForOrg(name, orgID)
	if err != nil {
		if errors.Is(err, service.ErrImageNotFound) {
			return util.ErrNotFound("Image not found")
		}
		return util.ErrInternal("Failed to fetch image", err)
	}

	c.JSON(http.StatusOK, model.NewImageResponse(image))
	return nil
}
