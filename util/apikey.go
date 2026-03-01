package util

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

const (
	// APIKeyPrefix is the prefix for organization API keys
	APIKeyPrefix = "org"
	// APIKeyLength is the length of the random part in bytes
	APIKeyLength = 32
	// BCryptCost is the cost factor for bcrypt hashing
	BCryptCost = 12
)

// GenerateAPIKey generates a new secure API key with format: org_<random_base64>
func GenerateAPIKey() (string, error) {
	randomBytes := make([]byte, APIKeyLength)
	_, err := rand.Read(randomBytes)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Use URL-safe base64 encoding without padding for cleaner keys
	randomPart := base64.RawURLEncoding.EncodeToString(randomBytes)
	apiKey := fmt.Sprintf("%s_%s", APIKeyPrefix, randomPart)

	return apiKey, nil
}

// HashAPIKey hashes an API key using bcrypt
func HashAPIKey(apiKey string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(apiKey), BCryptCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash API key: %w", err)
	}
	return string(hash), nil
}

// VerifyAPIKey compares a provided API key with its hash
func VerifyAPIKey(providedKey, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(providedKey))
	return err == nil
}
