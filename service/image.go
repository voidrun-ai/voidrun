package service

import (
	"context"
	"errors"
	"fmt"

	"voidrun/config"
	"voidrun/model"
	"voidrun/repository"
	"voidrun/util"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var ErrImageNotFound = errors.New("image not found")

// ImageService handles image-related business logic
type ImageService struct {
	repo repository.IImageRepository
	cfg  *config.Config
}

// NewImageService creates a new image service
func NewImageService(cfg *config.Config, repo repository.IImageRepository) *ImageService {
	return &ImageService{repo: repo, cfg: cfg}
}

// ListByOrg returns system images and images owned by the org.
func (s *ImageService) ListByOrg(ctx context.Context, orgID primitive.ObjectID) ([]*model.Image, error) {
	filter := bson.M{
		"$or": []bson.M{
			{"orgId": orgID},
			{"system": true, "active": true},
		},
	}

	opts := options.FindOptions{}
	opts.SetSort(bson.D{{Key: "_id", Value: -1}})
	return s.repo.Find(ctx, orgID, filter, opts)
}

// GetByOrg returns an image by ID when it is visible to the org.
func (s *ImageService) GetByOrg(ctx context.Context, orgID primitive.ObjectID, id string) (*model.Image, error) {
	imageID, err := util.ParseObjectID(id)
	if err != nil {
		return nil, fmt.Errorf("invalid image id: %w", err)
	}
	img, err := s.repo.FindByIDAndOrgOrSystem(ctx, imageID, orgID)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, ErrImageNotFound
	}
	return img, nil
}

// Create creates a new image
func (s *ImageService) Create(ctx context.Context, img *model.Image) (*model.Image, error) {
	return s.repo.Create(ctx, img)
}

// DeleteByOrg removes an org image by ID.
func (s *ImageService) DeleteByOrg(ctx context.Context, orgID primitive.ObjectID, id string) error {
	imageID, err := util.ParseObjectID(id)
	if err != nil {
		return fmt.Errorf("invalid image id: %w", err)
	}

	ok, err := s.repo.DeleteByIDAndOrg(ctx, imageID, orgID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrImageNotFound
	}
	return nil
}

// Exists checks if an image exists
func (s *ImageService) Exists(ctx context.Context, orgID primitive.ObjectID, id string) bool {
	objID, err := util.ParseObjectID(id)
	if err != nil {
		return false
	}
	return s.repo.Exists(ctx, orgID, objID)
}

// Count returns the number of images matching a filter
func (s *ImageService) Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error) {
	return s.repo.Count(ctx, orgID, filter)
}

// GetLatestByNameForOrg returns the latest image for a name visible to org.
func (s *ImageService) GetLatestByNameForOrg(name string, orgID primitive.ObjectID) (*model.Image, error) {
	img, err := s.repo.GetLatestByNameForOrg(name, orgID)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, ErrImageNotFound
	}
	return img, nil
}
