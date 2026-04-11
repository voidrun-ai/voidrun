package middleware

import "github.com/gin-gonic/gin"

// LocalModeMiddleware injects fixed identity values and bypasses authentication.
func LocalModeMiddleware(orgID, userID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("orgID", orgID)
		c.Set("userID", userID)
		c.Set("authMethod", "local")
		c.Next()
	}
}
