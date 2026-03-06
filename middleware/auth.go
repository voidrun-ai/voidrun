package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"voidrun/config"
	"voidrun/service"

	"github.com/gin-gonic/gin"
)

// Clerk token type header prefix
const clerkTokenTypePrefix = "Bearer "

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
func AuthMiddleware(cfg *config.Config, apiKeySvc *service.APIKeyService, userSvc *service.UserService, clerkSvc *service.ClerkService) gin.HandlerFunc {
	return func(c *gin.Context) {

		apiKey := c.GetHeader("X-API-Key")

		bearerToken := extractBearerToken(c.GetHeader("Authorization"))
		if apiKey != "" && bearerToken != "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "provide either X-API-Key or Bearer token, not both"})
			return
		}

		var orgID, userID string

		switch {
		case apiKey != "":
			orgID, userID = handleAPIKeyAuth(c, apiKeySvc, apiKey)
		case bearerToken != "":
			orgID = c.GetHeader("X-Org-ID")
			claims, err := clerkSvc.ValidateToken(c.Request.Context(), bearerToken)
			if err == nil && claims != nil {
				userID = handleClerkAuth(c, cfg, userSvc, claims)
			} else {
				fmt.Printf("[Auth] Clerk token validation failed: %v\n", err)
				// Clerk was enabled but token validation failed
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid clerk token"})
			}

		default:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required (X-API-Key or Bearer token)"})
		}

		c.Set("orgID", orgID)
		c.Set("userID", userID)
		c.Next()
	}
}

// handleClerkAuth handles authentication with Clerk JWT tokens
func handleClerkAuth(c *gin.Context, cfg *config.Config, userSvc *service.UserService, claims *service.ClerkClaims) string {
	// Get or create user based on Clerk ID
	clerkID := claims.Sub
	ctx := c.Request.Context()
	user, err := userSvc.CreateNewUserAndDefaultOrg(ctx, clerkID, claims)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to provision user"})
		return ""
	}
	return user.ID.Hex()
}

func handleAPIKeyAuth(c *gin.Context, apiKeySvc *service.APIKeyService, plainKey string) (string, string) {
	keyDoc, err := apiKeySvc.ValidateKey(c.Request.Context(), plainKey)
	if err != nil || keyDoc == nil || !keyDoc.IsActive {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or inactive API key"})
		return "", ""
	}

	resolvedOrgID := keyDoc.OrgID.Hex()

	return resolvedOrgID, keyDoc.CreatedBy.Hex()

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
