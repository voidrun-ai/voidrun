package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"voidrun/config"

	"github.com/redis/go-redis/v9"
)

const (
	apiKeyCacheKeyPrefix  = "auth:apikey:"
	apiKeyIDMapPrefix     = "auth:map_id:" // Maps KeyID -> plainKeySuffix
	clerkTokenCachePrefix = "auth:clerk:"
)

// AuthCacheEntry represents cached auth data
type AuthCacheEntry struct {
	UserID string `json:"userID"`
	OrgID  string `json:"orgID"`
	KeyID  string `json:"keyID"` // Database ID
}

// AuthCache wraps Redis client for auth caching
type AuthCache struct {
	client    redis.Cmdable
	apiKeyTTL time.Duration
	clerkTTL  time.Duration
}

// NewAuthCache creates a new AuthCache based on the Redis configuration
func NewAuthCache(cfg *config.Config) (*AuthCache, error) {
	if cfg.APIKeyCacheTTLSeconds <= 0 {
		cfg.APIKeyCacheTTLSeconds = 3600 // default 1 hour
	}
	if cfg.ClerkCacheTTLSeconds <= 0 {
		cfg.ClerkCacheTTLSeconds = 300 // default 5 minutes
	}

	var client redis.Cmdable

	mode := strings.ToLower(cfg.Redis.Mode)
	if mode == "" {
		mode = "single"
	}

	switch mode {
	case "cluster":
		addrs := strings.Split(cfg.Redis.ClusterAddrs, ",")
		if len(addrs) == 0 || (len(addrs) == 1 && addrs[0] == "") {
			return nil, fmt.Errorf("REDIS_CLUSTER_ADDRS is required for cluster mode")
		}
		// Clean up addresses
		cleanAddrs := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			addr = strings.TrimSpace(addr)
			if addr != "" {
				cleanAddrs = append(cleanAddrs, addr)
			}
		}
		if len(cleanAddrs) == 0 {
			return nil, fmt.Errorf("REDIS_CLUSTER_ADDRS is required for cluster mode")
		}

		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:    cleanAddrs,
			Password: cfg.Redis.Password,
		})

	case "sentinel":
		addrs := strings.Split(cfg.Redis.SentinelAddrs, ",")
		if len(addrs) == 0 || (len(addrs) == 1 && addrs[0] == "") {
			return nil, fmt.Errorf("REDIS_SENTINEL_ADDRS is required for sentinel mode")
		}
		if cfg.Redis.SentinelMaster == "" {
			return nil, fmt.Errorf("REDIS_SENTINEL_MASTER is required for sentinel mode")
		}
		// Clean up addresses
		cleanAddrs := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			addr = strings.TrimSpace(addr)
			if addr != "" {
				cleanAddrs = append(cleanAddrs, addr)
			}
		}
		if len(cleanAddrs) == 0 {
			return nil, fmt.Errorf("REDIS_SENTINEL_ADDRS is required for sentinel mode")
		}

		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.Redis.SentinelMaster,
			SentinelAddrs: cleanAddrs,
			Password:      cfg.Redis.Password,
			DB:            cfg.Redis.DB,
		})

	default: // "single"
		opts, parseErr := redis.ParseURL(cfg.Redis.URL)
		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse REDIS_URL: %w", parseErr)
		}
		if cfg.Redis.Password != "" {
			opts.Password = cfg.Redis.Password
		}
		opts.DB = cfg.Redis.DB
		client = redis.NewClient(opts)
	}

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &AuthCache{
		client:    client,
		apiKeyTTL: time.Duration(cfg.APIKeyCacheTTLSeconds) * time.Second,
		clerkTTL:  time.Duration(cfg.ClerkCacheTTLSeconds) * time.Second,
	}, nil
}

func key(plaintext string) string {
	return plaintext[len(plaintext)-32:] // use last 32 chars
}

// GetAPIKey retrieves cached auth data for an API key
func (ac *AuthCache) GetAPIKey(ctx context.Context, plainKey string) (*AuthCacheEntry, error) {
	if ac == nil || ac.client == nil {
		return nil, nil // Graceful degradation
	}

	key := apiKeyCacheKeyPrefix + key(plainKey)
	return ac.get(ctx, key)
}

// SetAPIKey caches auth data for an API key
// SetAPIKey caches auth data for an API key and creates an ID lookup mapping
func (ac *AuthCache) SetAPIKey(ctx context.Context, plainKey string, entry *AuthCacheEntry) error {
	if ac == nil || ac.client == nil || entry == nil {
		return nil // Graceful degradation
	}

	suffix := key(plainKey)
	dataKey := apiKeyCacheKeyPrefix + suffix
	idMapKey := apiKeyIDMapPrefix + entry.KeyID

	// 1. Store main data entry
	if err := ac.set(ctx, dataKey, entry, ac.apiKeyTTL); err != nil {
		return err
	}

	// 2. Store ID-to-Suffix mapping for invalidation
	return ac.client.Set(ctx, idMapKey, suffix, ac.apiKeyTTL).Err()
}

// DeleteAPIKey invalidates cached auth data for an API key
func (ac *AuthCache) DeleteAPIKey(ctx context.Context, plainKey string) error {
	if ac == nil || ac.client == nil {
		return nil // Graceful degradation
	}

	key := apiKeyCacheKeyPrefix + key(plainKey)
	return ac.delete(ctx, key)
}

// DeleteAPIKeyByID invalidates cached auth data using the database ID mapping
func (ac *AuthCache) DeleteAPIKeyByID(ctx context.Context, keyID string) error {
	if ac == nil || ac.client == nil || keyID == "" {
		return nil
	}

	idMapKey := apiKeyIDMapPrefix + keyID

	// 1. Get the suffix from the mapping
	suffix, err := ac.client.Get(ctx, idMapKey).Result()
	if err != nil {
		if err == redis.Nil {
			return nil // No mapping found, nothing to delete
		}
		return fmt.Errorf("failed to get ID mapping: %w", err)
	}

	// 2. Delete the main data entry
	dataKey := apiKeyCacheKeyPrefix + suffix
	_ = ac.delete(ctx, dataKey)

	// 3. Delete the mapping itself
	return ac.delete(ctx, idMapKey)
}

// GetClerkToken retrieves cached auth data for a Clerk token
func (ac *AuthCache) GetClerkToken(ctx context.Context, token string) (*AuthCacheEntry, error) {
	if ac == nil || ac.client == nil {
		return nil, nil // Graceful degradation
	}

	key := clerkTokenCachePrefix + key(token)
	return ac.get(ctx, key)
}

// SetClerkToken caches auth data for a Clerk token
func (ac *AuthCache) SetClerkToken(ctx context.Context, token string, entry *AuthCacheEntry) error {
	if ac == nil || ac.client == nil {
		return nil // Graceful degradation
	}

	key := clerkTokenCachePrefix + key(token)
	return ac.set(ctx, key, entry, ac.clerkTTL)
}

// DeleteClerkToken invalidates cached auth data for a Clerk token
func (ac *AuthCache) DeleteClerkToken(ctx context.Context, token string) error {
	if ac == nil || ac.client == nil {
		return nil // Graceful degradation
	}

	key := clerkTokenCachePrefix + key(token)
	return ac.delete(ctx, key)
}

// get retrieves cached auth data using raw Redis GET with JSON unmarshal
func (ac *AuthCache) get(ctx context.Context, key string) (*AuthCacheEntry, error) {
	data, err := ac.client.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // Cache miss
		}
		return nil, fmt.Errorf("cache get error: %w", err)
	}

	var entry AuthCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("cache unmarshal error: %w", err)
	}

	return &entry, nil
}

// set stores auth data using raw Redis SET with JSON marshal
func (ac *AuthCache) set(ctx context.Context, key string, entry *AuthCacheEntry, ttl time.Duration) error {
	if entry == nil {
		return nil
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cache marshal error: %w", err)
	}

	return ac.client.Set(ctx, key, data, ttl).Err()
}

// delete removes auth data from cache
func (ac *AuthCache) delete(ctx context.Context, key string) error {
	return ac.client.Del(ctx, key).Err()
}

// Close closes the Redis client
func (ac *AuthCache) Close() error {
	if ac == nil {
		return nil
	}

	if closer, ok := ac.client.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// Ping checks if Redis is available
func (ac *AuthCache) Ping(ctx context.Context) error {
	if ac == nil || ac.client == nil {
		return fmt.Errorf("auth cache is not initialized")
	}
	return ac.client.Ping(ctx).Err()
}
