package middleware

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"voidrun/config"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ctxKey is a private type for context keys
type ctxKey string

const (
	// Context keys used downstream
	CtxUserIDKey      ctxKey = "userID"
	CtxUserRoleKey    ctxKey = "userRole"
	CtxOrgIDKey       ctxKey = "orgID"
	CtxPermissionsKey ctxKey = "permissions"
)

const (
	maxAuthBodyBytes = 1 << 20 // 1 MiB
)

type jwtClaims struct {
	Sub    string `json:"sub"`
	UserID string `json:"userId"`
	Exp    int64  `json:"exp"`
}

// AuthMiddleware validates API Key or JWT and injects zero-trust org context.
func AuthMiddleware(cfg *config.Config, apiKeySvc *service.APIKeyService, orgSvc *service.OrgService, userSvc *service.UserService) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestedOrgID, err := extractRequestedOrgID(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		apiKey := strings.TrimSpace(c.GetHeader("X-API-Key"))
		if apiKey == "" {
			// WebSocket clients cannot set custom headers; allow query fallback.
			apiKey = strings.TrimSpace(c.Query("apiKey"))
		}

		bearerToken := extractBearerToken(c.GetHeader("Authorization"))
		if apiKey != "" && bearerToken != "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "provide either X-API-Key or Bearer token, not both"})
			return
		}

		switch {
		case apiKey != "":
			handleAPIKeyAuth(c, apiKeySvc, requestedOrgID, apiKey)
		case bearerToken != "":
			handleJWTAuth(c, cfg, orgSvc, userSvc, requestedOrgID, bearerToken)
		default:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required (X-API-Key or Bearer token)"})
		}
	}
}

func handleAPIKeyAuth(c *gin.Context, apiKeySvc *service.APIKeyService, requestedOrgID, plainKey string) {
	keyDoc, err := apiKeySvc.ValidateKey(c.Request.Context(), plainKey)
	if err != nil || keyDoc == nil || !keyDoc.IsActive {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or inactive API key"})
		return
	}

	resolvedOrgID := keyDoc.OrgID.Hex()
	if requestedOrgID != "" && requestedOrgID != resolvedOrgID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "requested organization does not match api key organization"})
		return
	}

	role := "org_api"
	userIDHex := ""
	if !keyDoc.CreatedBy.IsZero() {
		userIDHex = keyDoc.CreatedBy.Hex()
	}
	permissions := keyDoc.Scopes
	if len(permissions) == 0 {
		permissions = []string{"*"}
	}

	injectAuthContext(c, resolvedOrgID, userIDHex, role, permissions)
}

func handleJWTAuth(c *gin.Context, cfg *config.Config, orgSvc *service.OrgService, userSvc *service.UserService, requestedOrgID, token string) {
	if strings.TrimSpace(requestedOrgID) == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "x-org-id header (or matching org id in request) is required for jwt auth"})
		return
	}
	if !util.IsValidObjectID(requestedOrgID) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid organization id"})
		return
	}
	if strings.TrimSpace(cfg.Auth.JWTSecret) == "" {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "jwt auth is not configured"})
		return
	}

	claims, err := validateJWT(token, cfg.Auth.JWTSecret)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid jwt token"})
		return
	}

	userIDHex := strings.TrimSpace(claims.UserID)
	if userIDHex == "" {
		userIDHex = strings.TrimSpace(claims.Sub)
	}
	if !util.IsValidObjectID(userIDHex) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid jwt subject"})
		return
	}

	orgOID, _ := primitive.ObjectIDFromHex(requestedOrgID)
	userOID, _ := primitive.ObjectIDFromHex(userIDHex)

	hasAccess, err := orgSvc.UserHasAccess(c.Request.Context(), orgOID, userOID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to validate organization access"})
		return
	}
	if !hasAccess {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user is not a member of the requested organization"})
		return
	}

	user, err := userSvc.Me(c.Request.Context(), userIDHex)
	if err != nil || user == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}

	role := strings.TrimSpace(user.Role)
	if role == "" {
		role = "org_admin"
	}
	permissions := permissionsForRole(role)

	injectAuthContext(c, requestedOrgID, userIDHex, role, permissions)
}

func injectAuthContext(c *gin.Context, orgID, userID, role string, permissions []string) {
	ctx := c.Request.Context()
	ctx = context.WithValue(ctx, CtxOrgIDKey, orgID)
	ctx = context.WithValue(ctx, CtxUserIDKey, userID)
	ctx = context.WithValue(ctx, CtxUserRoleKey, role)
	ctx = context.WithValue(ctx, CtxPermissionsKey, permissions)

	// Also keep plain string keys for service-layer org scoping.
	ctx = context.WithValue(ctx, "orgID", orgID)
	ctx = context.WithValue(ctx, "userID", userID)
	ctx = context.WithValue(ctx, "role", role)
	ctx = context.WithValue(ctx, "permissions", permissions)
	c.Request = c.Request.WithContext(ctx)

	c.Set("orgID", orgID)
	c.Set("userID", userID)
	c.Set("role", role)
	c.Set("permissions", permissions)

	c.Next()
}

func permissionsForRole(role string) []string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "admin", "org_owner", "org_admin":
		return []string{"*"}
	case "org_viewer", "viewer":
		return []string{"org:read", "sandbox:read", "image:read", "apikey:read"}
	case "org_developer", "developer", "user":
		return []string{
			"org:read",
			"sandbox:read", "sandbox:create", "sandbox:update", "sandbox:exec", "sandbox:fs", "sandbox:pty",
			"image:read", "image:create",
			"apikey:read",
		}
	default:
		// Deny-by-default for unknown roles.
		return []string{}
	}
}

func extractBearerToken(authHeader string) string {
	authHeader = strings.TrimSpace(authHeader)
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func extractRequestedOrgID(c *gin.Context) (string, error) {
	candidates := []string{}

	addCandidate := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			candidates = append(candidates, raw)
		}
	}

	addCandidate(c.GetHeader("X-Org-ID"))
	addCandidate(c.Param("orgId"))
	addCandidate(c.Query("orgId"))

	bodyOrgID, err := extractBodyOrgID(c)
	if err != nil {
		return "", err
	}
	addCandidate(bodyOrgID)

	if len(candidates) == 0 {
		return "", nil
	}

	orgID := candidates[0]
	if !util.IsValidObjectID(orgID) {
		return "", errors.New("invalid organization id")
	}
	for _, candidate := range candidates[1:] {
		if candidate != orgID {
			return "", errors.New("conflicting organization ids in request")
		}
	}
	return orgID, nil
}

func extractBodyOrgID(c *gin.Context) (string, error) {
	if c.Request == nil || c.Request.Body == nil {
		return "", nil
	}

	contentType := strings.ToLower(strings.TrimSpace(c.ContentType()))
	if !strings.Contains(contentType, "application/json") {
		return "", nil
	}

	if c.Request.ContentLength < 0 || c.Request.ContentLength > maxAuthBodyBytes {
		return "", nil
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read request body")
	}
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	trimmed := bytes.TrimSpace(bodyBytes)
	if len(trimmed) == 0 {
		return "", nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		// Keep handler-level validation responsibility for malformed JSON.
		return "", nil
	}
	rawOrg, ok := payload["orgId"]
	if !ok || rawOrg == nil {
		return "", nil
	}

	orgID, ok := rawOrg.(string)
	if !ok {
		return "", errors.New("orgId must be a string")
	}
	return strings.TrimSpace(orgID), nil
}

func validateJWT(token, secret string) (*jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt structure")
	}

	headerPayload := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(headerPayload)); err != nil {
		return nil, err
	}
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(signature, expectedSig) {
		return nil, errors.New("jwt signature mismatch")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var header map[string]interface{}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, err
	}
	alg, _ := header["alg"].(string)
	if !strings.EqualFold(alg, "HS256") {
		return nil, errors.New("unsupported jwt algorithm")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, err
	}

	if claims.Exp == 0 || time.Now().Unix() >= claims.Exp {
		return nil, errors.New("jwt expired")
	}

	return &claims, nil
}
