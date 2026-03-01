package middleware

import (
	"context"
	"log"
	"net/http"

	"voidrun/internal/service"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
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
func AuthMiddleware(apiKeySvc *service.APIKeyService, orgSvc *service.OrgService) gin.HandlerFunc {
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

		resolvedOrgID := keyDoc.OrgID.Hex()
		userID := keyDoc.CreatedBy
		requestedOrgID := c.GetHeader("X-Org-ID")

		if requestedOrgID != "" && requestedOrgID != resolvedOrgID {
			if orgSvc == nil {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "organization override not supported"})
				return
			}
			requestedOrgOID, err := primitive.ObjectIDFromHex(requestedOrgID)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid organization id"})
				return
			}
			if userID.IsZero() {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "organization override requires user-bound API key"})
				return
			}
			hasAccess, err := orgSvc.UserHasAccess(c.Request.Context(), requestedOrgOID, userID)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to validate organization access"})
				return
			}
			if !hasAccess {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden organization"})
				return
			}
			resolvedOrgID = requestedOrgID
		}

		userIDHex := ""
		if !userID.IsZero() {
			userIDHex = userID.Hex()
		}
		log.Printf("Authenticated request using API key ID: %s for Org ID: %s", keyDoc.ID.Hex(), resolvedOrgID)

		// Inject orgID and a generic role for org API access
		ctx := context.WithValue(c.Request.Context(), CtxOrgIDKey, resolvedOrgID)
		ctx = context.WithValue(ctx, CtxUserRoleKey, "org_api")
		ctx = context.WithValue(ctx, CtxUserIDKey, userIDHex)
		c.Request = c.Request.WithContext(ctx)

		// Also expose in gin context for handlers that read from gin
		c.Set("orgID", resolvedOrgID)
		c.Set("role", "org_api")
		c.Set("userID", userIDHex)

		c.Next()
	}
}
