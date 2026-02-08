package service

import (
	"context"

	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/internal/repository"

	"go.mongodb.org/mongo-driver/mongo/options"
)

// ImageService handles image-related business logic
type ImageService struct {
	repo repository.IImageRepository
	cfg  *config.Config
}

// NewImageService creates a new image service
func NewImageService(cfg *config.Config, repo repository.IImageRepository) *ImageService {
	return &ImageService{repo: repo, cfg: cfg}
}

// List returns all images
func (s *ImageService) List(ctx context.Context) ([]*model.Image, error) {
	return s.repo.Find(ctx, nil, options.FindOptions{})
}

// Get returns an image by ID
func (s *ImageService) Get(ctx context.Context, id string) (*model.Image, error) {
	return s.repo.FindByID(ctx, id)
}

// Create creates a new image
func (s *ImageService) Create(ctx context.Context, img *model.Image) (*model.Image, error) {
	return s.repo.Create(ctx, img)
}

// Delete removes an image by ID
func (s *ImageService) Delete(ctx context.Context, id string) error {
	return s.repo.Delete(ctx, id)
}

// Exists checks if an image exists
func (s *ImageService) Exists(ctx context.Context, id string) bool {
	return s.repo.Exists(ctx, id)
}

// Count returns the number of images matching a filter
func (s *ImageService) Count(ctx context.Context, filter interface{}) (int64, error) {
	return s.repo.Count(ctx, filter)
}

// GetLatestByName returns the most recent image document for a given name
func (s *ImageService) GetLatestByName(name string) (*model.Image, error) {
	return s.repo.GetLatestByName(name)
}
