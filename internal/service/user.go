package service

import (
	"context"
	"errors"
	"time"
	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/internal/repository"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// SandboxService handles sandbox business logic
type UserService struct {
	repo repository.IUserRepository
	cfg  *config.Config
}

// NewUserService creates a new user service
func NewUserService(cfg *config.Config, repo repository.IUserRepository) *UserService {
	return &UserService{
		repo: repo,
		cfg:  cfg,
	}
}

func (s *UserService) Register(ctx context.Context, req *model.RegisterRequest) (*model.User, error) {
	// 1. Check duplicate
	existing, _ := s.repo.FindByEmail(ctx, req.Email)
	if existing != nil {
		return nil, errors.New("email already taken")
	}

	user := &model.User{
		Name:      req.Name,
		Email:     req.Email,
		CreatedAt: time.Now(),
	}

	err := s.repo.Create(ctx, user)
	return user, err
}

func (s *UserService) Me(ctx context.Context, userID string) (*model.User, error) {
	// Fetch user by ID
	userObjID, err := primitive.ObjectIDFromHex(userID)
	if err != nil {
		return nil, err
	}
	user, err := s.repo.FindByID(ctx, userObjID)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// GetByOrg returns all users belonging to an organization
func (s *UserService) GetByOrg(ctx context.Context, memberIDs []primitive.ObjectID) ([]*model.User, error) {
	return s.repo.FindByIDs(ctx, memberIDs)
}
