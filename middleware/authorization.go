package middleware

import (
	"net/http"

	"voidrun/model"

	"github.com/gin-gonic/gin"
)

// RequirePermission enforces action permissions after org/auth validation.
func RequirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawPerms, ok := c.Get("permissions")
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, model.NewErrorResponse("missing permissions", ""))
			return
		}

		perms, ok := rawPerms.([]string)
		if !ok || len(perms) == 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, model.NewErrorResponse("permission denied", permission))
			return
		}

		if hasPermission(perms, permission) {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusForbidden, model.NewErrorResponse("permission denied", permission))
	}
}

func hasPermission(perms []string, required string) bool {
	for _, p := range perms {
		if p == "*" || p == required {
			return true
		}
	}
	return false
}
