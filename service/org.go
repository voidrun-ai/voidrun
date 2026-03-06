package service

import (
	"context"

	"voidrun/model"
	"voidrun/repository"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// OrgService handles organization logic
type OrgService struct {
	repo repository.IOrgRepository
}

func NewOrgService(repo repository.IOrgRepository) *OrgService {
	return &OrgService{repo: repo}
}

// EnsureDefaultOrg checks for an owner org and creates one if missing
func (s *OrgService) EnsureDefaultOrg(ctx context.Context, ownerID primitive.ObjectID, name string) (*model.Org, error) {
	existing, err := s.repo.FindByOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	org := &model.Org{
		Name:      name,
		OwnerID:   ownerID,
		Members:   []primitive.ObjectID{ownerID},
		Plan:      "default",
		CreatedBy: ownerID,
	}
	return s.repo.Create(ctx, org)
}

// GetByID returns org by ObjectID
func (s *OrgService) GetByID(ctx context.Context, id primitive.ObjectID) (*model.Org, error) {
	return s.repo.FindByID(ctx, id)
}

// ListByMemberID returns organizations where user is a member.
func (s *OrgService) ListByMemberID(ctx context.Context, memberID primitive.ObjectID) ([]*model.Org, error) {
	return s.repo.FindByMember(ctx, memberID)
}

// UserHasAccess returns true when the user owns or is a member of the organization.
func (s *OrgService) UserHasAccess(ctx context.Context, orgID, userID primitive.ObjectID) (bool, error) {
	org, err := s.repo.FindByID(ctx, orgID)
	if err != nil {
		return false, err
	}
	if org == nil {
		return false, nil
	}
	if org.OwnerID == userID {
		return true, nil
	}
	for _, memberID := range org.Members {
		if memberID == userID {
			return true, nil
		}
	}
	return false, nil
}

// GetCurrentOrg returns the current org and all orgs the user has access to
func (s *OrgService) GetCurrentOrg(ctx context.Context, orgID primitive.ObjectID, userID primitive.ObjectID) ([]*model.Org, error) {
	org, err := s.repo.FindByID(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if org == nil {
		return nil, nil
	}

	// If no user ID provided, return just the current org
	if userID.IsZero() {
		return []*model.Org{org}, nil
	}

	// Get all orgs the user is a member of
	memberOrgs, err := s.repo.FindByMember(ctx, userID)
	if err != nil {
		return nil, err
	}

	return memberOrgs, nil
}
