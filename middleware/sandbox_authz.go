package middleware

import (
	"net/http"

	"voidrun/model"
	"voidrun/service"

	"github.com/gin-gonic/gin"
)

const sandboxCtxKey = "sandbox"

// SandboxAccessMiddleware validates that the sandbox in path belongs to the authenticated org.
func SandboxAccessMiddleware(sandboxSvc *service.SandboxService) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgIDVal, ok := c.Get("orgID")
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, model.NewErrorResponse("missing org context", ""))
			return
		}

		orgID, ok := orgIDVal.(string)
		if !ok || orgID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, model.NewErrorResponse("invalid org context", ""))
			return
		}

		id := c.Param("id")
		sandbox, found := sandboxSvc.Get(c.Request.Context(), id)
		if !found || sandbox == nil {
			c.AbortWithStatusJSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
			return
		}

		if sandbox.OrgID.Hex() != orgID {
			c.AbortWithStatusJSON(http.StatusForbidden, model.NewErrorResponse("Access denied", "sandbox does not belong to current organization"))
			return
		}

		c.Set(sandboxCtxKey, sandbox)
		c.Next()
	}
}
