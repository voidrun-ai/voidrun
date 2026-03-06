package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"voidrun/config"
	"voidrun/model"
	"voidrun/repository"
	"voidrun/util"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var ErrAPIKeyNotFound = errors.New("api key not found")

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
func (s *APIKeyService) ListByOrg(ctx context.Context, orgID string) ([]*model.APIKeyResponse, error) {
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
	orgID, err := orgIDFromContext(ctx)
	if err != nil {
		return err
	}
	return s.RevokeKeyByOrg(ctx, orgID, keyID)
}

// RevokeKeyByOrg deletes an API key only when it belongs to the provided org.
func (s *APIKeyService) RevokeKeyByOrg(ctx context.Context, orgID, keyID string) error {
	orgOID, err := primitive.ObjectIDFromHex(orgID)
	if err != nil {
		return fmt.Errorf("invalid org ID: %w", err)
	}
	keyOID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	ok, err := s.repo.DeleteByIDAndOrg(ctx, keyOID, orgOID)
	if err != nil {
		return fmt.Errorf("failed to revoke key: %w", err)
	}
	if !ok {
		return ErrAPIKeyNotFound
	}
	return nil
}

// DeactivateKey deactivates an API key (soft delete)
func (s *APIKeyService) DeactivateKey(ctx context.Context, keyID string) error {
	orgID, err := orgIDFromContext(ctx)
	if err != nil {
		return err
	}
	return s.DeactivateKeyByOrg(ctx, orgID, keyID)
}

// DeactivateKeyByOrg deactivates a key only when it belongs to the provided org.
func (s *APIKeyService) DeactivateKeyByOrg(ctx context.Context, orgID, keyID string) error {
	return s.setKeyActiveByOrg(ctx, orgID, keyID, false)
}

// ActivateKey reactivates a deactivated API key
func (s *APIKeyService) ActivateKey(ctx context.Context, keyID string) error {
	orgID, err := orgIDFromContext(ctx)
	if err != nil {
		return err
	}
	return s.ActivateKeyByOrg(ctx, orgID, keyID)
}

// ActivateKeyByOrg activates a key only when it belongs to the provided org.
func (s *APIKeyService) ActivateKeyByOrg(ctx context.Context, orgID, keyID string) error {
	return s.setKeyActiveByOrg(ctx, orgID, keyID, true)
}

// ValidateKey verifies a plain key against stored hash and updates last used
func (s *APIKeyService) ValidateKey(ctx context.Context, plainKey string) (*model.APIKey, error) {
	defer util.Track("Validate Auth Key (Total)")()
	// Check cache first
	s.keyCacheMutex.RLock()
	if entry, exists := s.keyCache[plainKey]; exists && time.Now().Before(entry.expiresAt) {
		s.keyCacheMutex.RUnlock()

		// Revalidate cached key state so revocations are effective immediately.
		fresh, err := s.repo.FindByIDAndOrg(ctx, entry.key.ID, entry.key.OrgID)
		if err != nil || fresh == nil || !fresh.IsActive {
			s.keyCacheMutex.Lock()
			delete(s.keyCache, plainKey)
			s.keyCacheMutex.Unlock()
			return nil, fmt.Errorf("invalid api key")
		}

		_ = s.repo.UpdateLastUsed(ctx, fresh.ID, fresh.OrgID)
		return fresh, nil
	}
	s.keyCacheMutex.RUnlock()

	// Cache miss or expired: query database
	keys, err := s.repo.FindActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to validate key: %w", err)
	}

	for _, key := range keys {
		if util.VerifyAPIKey(plainKey, key.Hash) {
			_ = s.repo.UpdateLastUsed(ctx, key.ID, key.OrgID)

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

func (s *APIKeyService) setKeyActiveByOrg(ctx context.Context, orgID, keyID string, isActive bool) error {
	orgOID, err := primitive.ObjectIDFromHex(orgID)
	if err != nil {
		return fmt.Errorf("invalid org ID: %w", err)
	}
	keyOID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return fmt.Errorf("invalid key ID: %w", err)
	}

	ok, err := s.repo.UpdateByIDAndOrg(ctx, keyOID, orgOID, map[string]interface{}{"isActive": isActive})
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
