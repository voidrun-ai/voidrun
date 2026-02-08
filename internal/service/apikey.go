package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/internal/repository"
	"voidrun/pkg/timer"
	"voidrun/pkg/util"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// APIKeyCache entry with expiration
type apiKeyCacheEntry struct {
	key       *model.APIKey
	expiresAt time.Time
}

// APIKeyService handles API key business logic
type APIKeyService struct {
	repo          repository.IAPIKeyRepository
	cfg           *config.Config
	keyCache      map[string]*apiKeyCacheEntry // plainKey -> cached result
	keyCacheMutex sync.RWMutex
	cacheTTL      time.Duration
}

// NewAPIKeyService creates a new API key service
func NewAPIKeyService(repo repository.IAPIKeyRepository, cfg *config.Config) *APIKeyService {
	cacheSeconds := cfg.APIKeyCacheTTLSeconds
	if cacheSeconds <= 0 {
		cacheSeconds = 300 // fallback to 5 minutes if misconfigured
	}

	return &APIKeyService{
		repo:     repo,
		cfg:      cfg,
		keyCache: make(map[string]*apiKeyCacheEntry),
		cacheTTL: time.Duration(cacheSeconds) * time.Second,
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
		Hash:      hash,
		CreatedBy: userID,
		CreatedAt: time.Now(),
		IsActive:  true,
		UpdatedAt: time.Now(),
	}

	created, err := s.repo.Create(ctx, apiKey)
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

// GenerateKeyFromStrings helper that parses string IDs (for handlers)
func (s *APIKeyService) GenerateKeyFromStrings(ctx context.Context, orgIDHex, userIDHex, keyName string) (*model.GeneratedAPIKeyResponse, error) {
	orgID, err := primitive.ObjectIDFromHex(orgIDHex)
	if err != nil {
		return nil, fmt.Errorf("invalid org ID: %w", err)
	}

	userID := primitive.NilObjectID
	if userIDHex != "" {
		if uid, err := primitive.ObjectIDFromHex(userIDHex); err == nil {
			userID = uid
		}
	}

	return s.GenerateKey(ctx, orgID, userID, keyName)
}

// GetByID retrieves an API key by ID
func (s *APIKeyService) GetByID(ctx context.Context, id string) (*model.APIKeyResponse, error) {
	objID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return nil, fmt.Errorf("invalid key ID: %w", err)
	}

	apiKey, err := s.repo.FindByID(ctx, objID)
	if err != nil {
		return nil, fmt.Errorf("key not found: %w", err)
	}

	resp := apiKey.ToResponse()
	return &resp, nil
}

// ListByOrgID retrieves all API keys for an organization
func (s *APIKeyService) ListByOrgID(ctx context.Context, orgID string) ([]*model.APIKeyResponse, error) {
	objID, err := primitive.ObjectIDFromHex(orgID)
	if err != nil {
		return nil, fmt.Errorf("invalid org ID: %w", err)
	}

	apiKeys, err := s.repo.FindByOrgID(ctx, objID)
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

// RevokeKey deletes an API key
func (s *APIKeyService) RevokeKey(ctx context.Context, keyID string) error {
	objID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	if err := s.repo.Delete(ctx, objID); err != nil {
		return fmt.Errorf("failed to revoke key: %w", err)
	}

	return nil
}

// DeactivateKey deactivates an API key (soft delete)
func (s *APIKeyService) DeactivateKey(ctx context.Context, keyID string) error {
	objID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	if err := s.repo.Update(ctx, objID, map[string]interface{}{"isActive": false}); err != nil {
		return fmt.Errorf("failed to deactivate key: %w", err)
	}

	return nil
}

// ActivateKey reactivates a deactivated API key
func (s *APIKeyService) ActivateKey(ctx context.Context, keyID string) error {
	objID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	if err := s.repo.Update(ctx, objID, map[string]interface{}{"isActive": true}); err != nil {
		return fmt.Errorf("failed to activate key: %w", err)
	}

	return nil
}

// ValidateKey verifies a plain key against stored hash and updates last used
func (s *APIKeyService) ValidateKey(ctx context.Context, plainKey string) (*model.APIKey, error) {
	defer timer.Track("Validate Auth Key (Total)")()
	// Check cache first
	s.keyCacheMutex.RLock()
	if entry, exists := s.keyCache[plainKey]; exists && time.Now().Before(entry.expiresAt) {
		s.keyCacheMutex.RUnlock()
		_ = s.repo.UpdateLastUsed(ctx, entry.key.ID)
		return entry.key, nil
	}
	s.keyCacheMutex.RUnlock()

	// Cache miss or expired: query database
	keys, err := s.repo.FindActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to validate key: %w", err)
	}

	for _, key := range keys {
		if util.VerifyAPIKey(plainKey, key.Hash) {
			_ = s.repo.UpdateLastUsed(ctx, key.ID)

			// Cache the valid key
			s.keyCacheMutex.Lock()
			s.keyCache[plainKey] = &apiKeyCacheEntry{
				key:       key,
				expiresAt: time.Now().Add(s.cacheTTL),
			}
			s.keyCacheMutex.Unlock()

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
			_ = s.repo.UpdateLastUsed(ctx, key.ID)
			return true, nil
		}
	}

	return false, nil
}

// TouchKey updates the last-used timestamp
func (s *APIKeyService) TouchKey(ctx context.Context, keyID string, t time.Time) error {
	objID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}
	return s.repo.Update(ctx, objID, map[string]interface{}{
		"lastUsedAt": t,
	})
}

// GetKeyCount returns the number of API keys for an organization
func (s *APIKeyService) GetKeyCount(ctx context.Context, orgID string) (int64, error) {
	objID, err := primitive.ObjectIDFromHex(orgID)
	if err != nil {
		return 0, fmt.Errorf("invalid org ID: %w", err)
	}

	count, err := s.repo.Count(ctx, map[string]interface{}{"orgId": objID})
	return count, err
}
