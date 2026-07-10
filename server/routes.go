package server

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// RouteID is "METHOD /full/gin/path" with gin placeholders kept (e.g. "/api/sandboxes/:id/exec").
type RouteID string

// RouteIDFor returns the canonical ID for method and full gin path (c.FullPath() or BasePath()+relative).
func RouteIDFor(method, fullPath string) RouteID {
	return RouteID(strings.ToUpper(method) + " " + fullPath)
}

// AnyMethod is the pseudo-method for gin.Any() routes (e.g. MCP).
const AnyMethod = "ANY"

// RouteAll ("*") runs on every protected route before per-route middleware; cannot collide with RouteIDFor IDs.
const RouteAll RouteID = "*"

// RouteMiddlewares maps RouteID to middleware after global auth and before the handler.
// Valid IDs come from setupRouter mount() calls; unknown IDs fail at boot.
type RouteMiddlewares map[RouteID][]gin.HandlerFunc

// Wrap prepends the injected chain for routeID and returns a new slice (avoids append aliasing the map).
func (rm RouteMiddlewares) Wrap(routeID RouteID, final gin.HandlerFunc) []gin.HandlerFunc {
	injected := rm[routeID]
	out := make([]gin.HandlerFunc, 0, len(injected)+1)
	out = append(out, injected...)
	out = append(out, final)
	return out
}
