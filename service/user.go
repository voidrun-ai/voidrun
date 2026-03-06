package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"voidrun/config"
	"voidrun/model"
	"voidrun/repository"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// UserService handles sandbox business logic
type UserService struct {
	repo     repository.IUserRepository
	cfg      *config.Config
	clerkSvc *ClerkService
	orgSvc   *OrgService
}

// NewUserService creates a new user service
func NewUserService(cfg *config.Config, repo repository.IUserRepository, clerkSvc *ClerkService, orgSvc *OrgService) *UserService {
	return &UserService{
		repo:     repo,
		cfg:      cfg,
		clerkSvc: clerkSvc,
		orgSvc:   orgSvc,
	}
}

// func (s *UserService) Register(ctx context.Context, req *model.RegisterRequest) (*model.User, error) {
// 	// 1. Check duplicate
// 	existing, _ := s.repo.FindByEmail(ctx, req.Email)
// 	if existing != nil {
// 		return nil, errors.New("email already taken")
// 	}

// 	user := &model.User{
// 		Name:      req.Name,
// 		Email:     req.Email,
// 		Role:      "org_admin",
// 		CreatedAt: time.Now(),
// 	}

// 	user, err := s.repo.Create(ctx, user)
// 	return user, err
// }

func (s *UserService) GetByID(ctx context.Context, userID string) (*model.User, error) {
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

// FindOrCreateByClerkExtID finds or creates a user based on their Clerk external ID
func (s *UserService) FindOrCreateByClerkExtID(ctx context.Context, userId, email, name string) (*model.User, error) {
	userIdBson, err := primitive.ObjectIDFromHex(userId)
	if err != nil {
		return nil, err
	}
	user, err := s.repo.FindByID(ctx, userIdBson)
	if err != nil {
		return nil, err
	} else if user != nil {
		return user, nil
	}

	// Create new user from Clerk data
	user = &model.User{
		Name:      deriveName(name, email),
		Email:     email,
		CreatedAt: time.Now(),
	}

	user, err = s.repo.Create(ctx, user)

	if err != nil {
		return nil, err
	}

	// Update external ID in Clerk
	if s.clerkSvc != nil {
		err = s.clerkSvc.UpdateExternalID(ctx, userId, user.ID.Hex())
		if err != nil {
			return nil, fmt.Errorf("failed to update clerk external ID: %w", err)
		}
	}

	return user, nil
}

func (s *UserService) CreateNewUserAndDefaultOrg(ctx context.Context, clerkID string, claims *ClerkClaims) (*model.User, error) {
	user, err := s.repo.FindOne(ctx, bson.M{"clerkId": clerkID}, nil)
	if err != nil {
		return nil, err
	}

	if user == nil {
		// Create new user from Clerk data
		user := &model.User{
			Name:  deriveName(claims.Name, claims.Email),
			Email: claims.Email,

			CreatedAt: time.Now(),
			ClerkID:   clerkID,
			ImageURL:  claims.Picture,
		}

		user, err = s.repo.Create(ctx, user)

		if err != nil {
			return nil, err
		}

		// create default org for this user
		_, err = s.orgSvc.EnsureDefaultOrg(ctx, user.ID, strings.ReplaceAll(strings.Split(user.Name, " ")[0]+"-Org", " ", "-"))
		if err != nil {
			return nil, fmt.Errorf("failed to create default organization: %w", err)
		}

		// Update external ID in Clerk
		if s.clerkSvc != nil {
			err = s.clerkSvc.UpdateExternalID(ctx, clerkID, user.ID.Hex())
			if err != nil {
				return nil, fmt.Errorf("failed to update clerk external ID: %w", err)
			}
		}

		return user, nil
	}

	return user, nil

}

// deriveName creates a name from email if name is empty
func deriveName(name, email string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	if email != "" {
		parts := strings.Split(email, "@")
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return "User"
}
