package middleware

import (
	"context"
	"log"
	"net/http"

	"voidrun/internal/service"

	"github.com/gin-gonic/gin"
)

// ctxKey is a private type for context keys
type ctxKey string

const (
	// Context keys used downstream
	CtxUserIDKey   ctxKey = "userID"
	CtxUserRoleKey ctxKey = "userRole"
	CtxOrgIDKey    ctxKey = "orgID"
)

// AuthMiddleware validates the X-API-Key header and injects org context.
// For now, it requires a valid organization API key.
func AuthMiddleware(apiKeySvc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("X-API-Key")
		if apiKey == "" {
			// WebSocket clients cannot set custom headers; allow query fallback for interactive shells
			apiKey = c.Query("apiKey")
		}
		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "X-API-Key header required"})
			return
		}

		keyDoc, err := apiKeySvc.ValidateKey(c.Request.Context(), apiKey)
		if err != nil || keyDoc == nil || !keyDoc.IsActive {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or inactive API key"})
			return
		}

		orgId := keyDoc.OrgID.Hex()
		log.Printf("Authenticated request using API key ID: %s for Org ID: %s", keyDoc.ID.Hex(), orgId)

		// Inject orgID and a generic role for org API access
		ctx := context.WithValue(c.Request.Context(), CtxOrgIDKey, orgId)
		ctx = context.WithValue(ctx, CtxUserRoleKey, "org_api")
		c.Request = c.Request.WithContext(ctx)

		// Also expose in gin context for handlers that read from gin
		c.Set("orgID", orgId)
		c.Set("role", "org_api")

		c.Next()
	}
}
