package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const PolicyCacheTTL = 30 * time.Second

type policyEntry struct {
	policy  *model.NetworkPolicy
	secrets []model.SecretConfig
	fetchedAt time.Time
}

// PolicyCache holds sandbox network policies in memory, backed by MongoDB.
// The cache key is the sandbox IP address (the source IP the proxy sees on the veth).
type PolicyCache struct {
	mu    sync.RWMutex
	store map[string]*policyEntry
	coll  *mongo.Collection
}

// NewPolicyCache creates a policy cache backed by the given sandboxes collection.
func NewPolicyCache(sandboxesCollection *mongo.Collection) *PolicyCache {
	return &PolicyCache{
		store: make(map[string]*policyEntry),
		coll:  sandboxesCollection,
	}
}

// Get returns the NetworkPolicy for a sandbox identified by its IP address.
// SecretMappings are populated at runtime by resolving host environment variables.
// Returns nil, nil when the sandbox is not found in MongoDB.
// Returns nil, err when MongoDB is unreachable.
func (c *PolicyCache) Get(ctx context.Context, vmIP string) (*model.NetworkPolicy, error) {
	// Check in-memory cache first (read lock)
	c.mu.RLock()
	e, ok := c.store[vmIP]
	c.mu.RUnlock()

	if ok && time.Since(e.fetchedAt) < PolicyCacheTTL {
		return c.resolveSecrets(e), nil
	}

	// Cache miss — fetch from MongoDB, projecting network_policy + secrets
	var result struct {
		NetworkPolicy *model.NetworkPolicy `bson:"network_policy"`
		Secrets       []model.SecretConfig `bson:"secrets"`
	}
	err := c.coll.FindOne(
		ctx,
		bson.M{"ip": vmIP},
		options.FindOne().SetProjection(bson.M{
			"network_policy": 1,
			"secrets":        1,
			"_id":            0,
		}),
	).Decode(&result)

	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mongodb fetch policy for %s: %w", vmIP, err)
	}

	// Store in cache under write lock
	entry := &policyEntry{
		policy:    result.NetworkPolicy,
		secrets:   result.Secrets,
		fetchedAt: time.Now(),
	}
	c.mu.Lock()
	c.store[vmIP] = entry
	c.mu.Unlock()

	return c.resolveSecrets(entry), nil
}

// resolveSecrets populates SecretMappings on the policy by reading real values
// from the host's environment variables. Values are resolved on every call
// (not cached) so that rotated env vars take effect without cache invalidation.
func (c *PolicyCache) resolveSecrets(e *policyEntry) *model.NetworkPolicy {
	if e.policy == nil {
		return nil
	}

	// Make a shallow copy to avoid mutating the cached policy
	pol := *e.policy
	pol.SecretMappings = nil

	if len(e.secrets) == 0 {
		return &pol
	}

	mappings := make([]model.SecretMapping, 0, len(e.secrets))
	for _, s := range e.secrets {
		value := os.Getenv(s.FromEnvVar)
		if value == "" {
			// Skip secrets whose host env var is not set
			continue
		}
		mappings = append(mappings, model.SecretMapping{
			Placeholder: s.Placeholder,
			Value:       value,
			Hosts:       s.Hosts,
		})
	}
	pol.SecretMappings = mappings
	return &pol
}

// Invalidate removes a sandbox's policy from the in-memory cache.
// Call after updating network_policy in MongoDB so the next request re-fetches.
func (c *PolicyCache) Invalidate(vmIP string) {
	c.mu.Lock()
	delete(c.store, vmIP)
	c.mu.Unlock()
}
