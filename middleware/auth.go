package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"voidrun/config"
	"voidrun/service"

	"github.com/gin-gonic/gin"
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

// AuthMiddleware validates API Key or JWT and injects zero-trust org context.
func AuthMiddleware(cfg *config.Config, apiKeySvc *service.APIKeyService, userSvc *service.UserService, clerkSvc *service.ClerkService, authCache *service.AuthCache) gin.HandlerFunc {
	return func(c *gin.Context) {

		apiKey := c.GetHeader("X-API-Key")
		bearerToken := extractBearerToken(c.GetHeader("Authorization"))
		if apiKey == "" && bearerToken == "" {
			if c.IsWebsocket() {
				// ws will send api key to query param
				apiKey = c.Query("apiKey")
				bearerToken = c.Query("token")
			}

			if apiKey != "" && bearerToken != "" {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "provide either X-API-Key or Bearer token, not both"})
				return
			}
		}

		var orgID, userID, authMethod string

		switch {
		case apiKey != "":
			orgID, userID = handleAPIKeyAuth(c, apiKeySvc, authCache, apiKey)
			authMethod = "apikey"
		case bearerToken != "":
			orgID, userID = handleClerkAuth(c, userSvc, clerkSvc, authCache, bearerToken)
			authMethod = "token"

		default:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required (X-API-Key or Bearer token)"})
			return
		}

		// If auth handler aborted (e.g. invalid key/token), don't continue
		if c.IsAborted() {
			return
		}

		c.Set("orgID", orgID)
		c.Set("userID", userID)
		c.Set("authMethod", authMethod)
		c.Next()
	}
}

// handleClerkAuth handles authentication with Clerk JWT tokens.
//
// orgID is always taken from the request's X-Org-ID header, never from the
// auth cache. The cache stores only the (token → userID) mapping, because a
// single Clerk session may be reused across requests that target different
// orgs (or none at all, e.g. /users/me). Caching orgID caused stale-empty-org
// 401s when a request without X-Org-ID populated the cache first.
func handleClerkAuth(c *gin.Context, userSvc *service.UserService, clerkSvc *service.ClerkService, authCache *service.AuthCache, token string) (string, string) {
	ctx := c.Request.Context()
	orgID := c.GetHeader("X-Org-ID")

	if authCache != nil {
		cachedEntry, err := authCache.GetClerkToken(ctx, token)
		if err != nil {
			fmt.Printf("[Auth] Clerk cache get error: %v\n", err)
		} else if cachedEntry != nil && cachedEntry.UserID != "" {
			return orgID, cachedEntry.UserID
		}
		fmt.Printf("[Auth] Clerk cache MISS - validating token\n")
	} else {
		fmt.Printf("[Auth] Clerk cache DISABLED - validating token directly\n")
	}

	claims, err := clerkSvc.ValidateToken(c.Request.Context(), token)
	if err != nil || claims == nil {
		fmt.Printf("[Auth] Clerk token validation failed: %v\n", err)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid clerk token"})
		return "", ""
	}

	clerkID := claims.Sub
	user, err := userSvc.CreateNewUserAndDefaultOrg(ctx, clerkID, claims)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to provision user"})
		return "", ""
	}

	userID := user.ID.Hex()

	if authCache != nil {
		if err := authCache.SetClerkToken(ctx, token, &service.AuthCacheEntry{
			UserID: userID,
		}); err != nil {
			fmt.Printf("[Auth] Clerk cache set error: %v\n", err)
		} else {
			fmt.Printf("[Auth] Clerk cache SET - userID: %s\n", userID)
		}
	}

	return orgID, userID
}

func handleAPIKeyAuth(c *gin.Context, apiKeySvc *service.APIKeyService, authCache *service.AuthCache, plainKey string) (string, string) {
	ctx := c.Request.Context()

	// Try cache first
	if authCache != nil {
		cachedEntry, err := authCache.GetAPIKey(ctx, plainKey)
		if err != nil {
			fmt.Printf("[Auth] API key cache get error: %v\n", err)
		} else if cachedEntry != nil {
			// Cache hit - use cached userID and orgID
			// fmt.Printf("[Auth] API key cache HIT - userID: %s, orgID: %s\n", cachedEntry.UserID, cachedEntry.OrgID)
			return cachedEntry.OrgID, cachedEntry.UserID
		}
		fmt.Printf("[Auth] API key cache MISS - validating key\n")
	} else {
		fmt.Printf("[Auth] API key cache DISABLED - validating key directly\n")
	}

	// Cache miss - validate via database
	keyDoc, err := apiKeySvc.ValidateKey(ctx, plainKey)
	if err != nil || keyDoc == nil || !keyDoc.IsActive {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or inactive API key"})
		return "", ""
	}

	resolvedOrgID := keyDoc.OrgID.Hex()
	userID := keyDoc.CreatedBy.Hex()

	// Cache the result
	if authCache != nil {
		if err := authCache.SetAPIKey(ctx, plainKey, &service.AuthCacheEntry{
			UserID: userID,
			OrgID:  resolvedOrgID,
			KeyID:  keyDoc.ID.Hex(),
		}); err != nil {
			fmt.Printf("[Auth] API key cache set error: %v\n", err)
		} else {
			fmt.Printf("[Auth] API key cache SET - userID: %s, orgID: %s\n", userID, resolvedOrgID)
		}
	}

	return resolvedOrgID, userID
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
