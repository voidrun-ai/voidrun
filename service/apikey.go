package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"voidrun/config"
	"voidrun/model"
	"voidrun/repository"
	"voidrun/util"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var ErrAPIKeyNotFound = errors.New("api key not found")

// APIKeyService handles API key business logic
type APIKeyService struct {
	repo      repository.IAPIKeyRepository
	cfg       *config.Config
	authCache *AuthCache
}

// NewAPIKeyService creates a new API key service
func NewAPIKeyService(repo repository.IAPIKeyRepository, cfg *config.Config, authCache *AuthCache) *APIKeyService {
	return &APIKeyService{
		repo:      repo,
		cfg:       cfg,
		authCache: authCache,
	}
}

func generateAndHash() (plainKey string, hash string, err error) {
	plainKey, err = util.GenerateAPIKey()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate key: %w", err)
	}

	hash, err = util.HashAPIKey(plainKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to hash key: %w", err)
	}

	return plainKey, hash, nil
}

// GenerateKey creates a new API key for an organization
func (s *APIKeyService) GenerateKey(ctx context.Context, orgID, userID primitive.ObjectID, keyName string) (*model.GeneratedAPIKeyResponse, error) {
	plainKey, hash, err := generateAndHash()
	if err != nil {
		return nil, err
	}

	apiKey := &model.APIKey{
		OrgID:     orgID,
		Name:      keyName,
		Scopes:    []string{"*"},
		Hash:      hash,
		CreatedBy: userID,
		CreatedAt: time.Now(),
		IsActive:  true,
		UpdatedAt: time.Now(),
	}

	created, err := s.repo.Create(ctx, orgID, apiKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create API key: %w", err)
	}

	return &model.GeneratedAPIKeyResponse{
		PlainKey:  plainKey,
		KeyID:     created.ID.Hex(),
		KeyName:   created.Name,
		OrgID:     orgID.Hex(),
		CreatedAt: created.CreatedAt,
		ExpiresIn: "Never (until revoked)",
	}, nil
}

// GetByID retrieves an API key by ID
func (s *APIKeyService) GetByID(ctx context.Context, id string) (*model.APIKeyResponse, error) {
	orgIDHex, err := orgIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	orgID, err := primitive.ObjectIDFromHex(orgIDHex)
	if err != nil {
		return nil, fmt.Errorf("invalid org ID: %w", err)
	}

	objID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return nil, fmt.Errorf("invalid key ID: %w", err)
	}

	apiKey, err := s.repo.FindByIDAndOrg(ctx, objID, orgID)
	if err != nil {
		return nil, fmt.Errorf("key not found: %w", err)
	}
	if apiKey == nil {
		return nil, ErrAPIKeyNotFound
	}

	resp := apiKey.ToResponse()
	return &resp, nil
}

// ListByOrgID retrieves all API keys for an organization
func (s *APIKeyService) ListByOrg(ctx context.Context, orgID primitive.ObjectID) ([]*model.APIKeyResponse, error) {

	apiKeys, err := s.repo.FindByOrgID(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list keys: %w", err)
	}

	responses := make([]*model.APIKeyResponse, len(apiKeys))
	for i, key := range apiKeys {
		resp := key.ToResponse()
		responses[i] = &resp
	}

	return responses, nil
}

// RevokeKeyByOrg deletes an API key only when it belongs to the provided org.
func (s *APIKeyService) RevokeKeyByOrg(ctx context.Context, orgID primitive.ObjectID, keyID string) error {
	keyOID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	ok, err := s.repo.DeleteByIDAndOrg(ctx, keyOID, orgID)
	if err != nil {
		return fmt.Errorf("failed to revoke key: %w", err)
	}
	if !ok {
		return ErrAPIKeyNotFound
	}

	// Note: Cache invalidation for the revoked key is handled at the middleware level
	// since we don't have access to the plain key here (only the hash is stored)
	return nil
}

// DeactivateKeyByOrg deactivates a key only when it belongs to the provided org.
func (s *APIKeyService) DeactivateKeyByOrg(ctx context.Context, orgID primitive.ObjectID, keyID string) error {
	return s.setKeyActiveByOrg(ctx, orgID, keyID, false)
}

// ActivateKeyByOrg activates a key only when it belongs to the provided org.
func (s *APIKeyService) ActivateKeyByOrg(ctx context.Context, orgID primitive.ObjectID, keyID string) error {
	return s.setKeyActiveByOrg(ctx, orgID, keyID, true)
}

// ValidateKey verifies a plain key against stored hash and updates last used
// Note: Caching is now handled at the middleware level via AuthCache
func (s *APIKeyService) ValidateKey(ctx context.Context, plainKey string) (*model.APIKey, error) {
	defer util.Track("Validate Auth Key (Total)")()

	// Query database for all active keys
	keys, err := s.repo.FindActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to validate key: %w", err)
	}

	for _, key := range keys {
		if util.VerifyAPIKey(plainKey, key.Hash) {
			_ = s.repo.UpdateLastUsed(ctx, key.ID, key.OrgID)
			return key, nil
		}
	}

	return nil, fmt.Errorf("invalid api key")
}

// ValidateKeyForOrg validates an API key for a specific organization
func (s *APIKeyService) ValidateKeyForOrg(ctx context.Context, plainKey string, orgID primitive.ObjectID) (bool, error) {
	// Get all active keys for the org
	keys, err := s.repo.FindByOrgID(ctx, orgID)
	if err != nil {
		return false, err
	}

	// Check if any key matches
	for _, key := range keys {
		if !key.IsActive {
			continue
		}

		if util.VerifyAPIKey(plainKey, key.Hash) {
			// Update last used
			_ = s.repo.UpdateLastUsed(ctx, key.ID, key.OrgID)
			return true, nil
		}
	}

	return false, nil
}

// TouchKey updates the last-used timestamp
func (s *APIKeyService) TouchKey(ctx context.Context, keyID string, t time.Time) error {
	orgID, err := orgIDFromContext(ctx)
	if err != nil {
		return err
	}
	return s.TouchKeyByOrg(ctx, orgID, keyID, t)
}

// TouchKeyByOrg updates last-used timestamp only when key belongs to org.
func (s *APIKeyService) TouchKeyByOrg(ctx context.Context, orgID, keyID string, t time.Time) error {
	orgOID, err := primitive.ObjectIDFromHex(orgID)
	if err != nil {
		return fmt.Errorf("invalid org ID: %w", err)
	}
	keyOID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	ok, err := s.repo.UpdateByIDAndOrg(ctx, keyOID, orgOID, map[string]interface{}{
		"lastUsedAt": t,
	})
	if err != nil {
		return fmt.Errorf("failed to touch key: %w", err)
	}
	if !ok {
		return ErrAPIKeyNotFound
	}
	return nil
}

func (s *APIKeyService) setKeyActiveByOrg(ctx context.Context, orgID primitive.ObjectID, keyID string, isActive bool) error {
	keyOID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	ok, err := s.repo.UpdateByIDAndOrg(ctx, keyOID, orgID, map[string]interface{}{"isActive": isActive})
	if err != nil {
		if isActive {
			return fmt.Errorf("failed to activate key: %w", err)
		}
		return fmt.Errorf("failed to deactivate key: %w", err)
	}
	if !ok {
		return ErrAPIKeyNotFound
	}
	return nil
}

func orgIDFromContext(ctx context.Context) (string, error) {
	raw := ctx.Value("orgID")
	orgID, ok := raw.(string)
	if !ok || strings.TrimSpace(orgID) == "" {
		return "", fmt.Errorf("missing org context")
	}
	return orgID, nil
}

// GetKeyCount returns the number of API keys for an organization
func (s *APIKeyService) GetKeyCount(ctx context.Context, orgID string) (int64, error) {
	objID, err := primitive.ObjectIDFromHex(orgID)
	if err != nil {
		return 0, fmt.Errorf("invalid org ID: %w", err)
	}

	count, err := s.repo.Count(ctx, objID, map[string]interface{}{})
	return count, err
}

// InvalidateAPIKeyCache invalidates the cache for a specific API key
// This is called when a key is revoked or deactivated
func (s *APIKeyService) InvalidateAPIKeyCache(ctx context.Context, plainKey string) {
	if s.authCache != nil {
		if err := s.authCache.DeleteAPIKey(ctx, plainKey); err != nil {
			fmt.Printf("[APIKey] Failed to invalidate cache: %v\n", err)
		}
	}
}
