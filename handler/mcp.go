package handler

import (
	"context"

	mcppkg "voidrun/mcp"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// MCPHandler bridges Gin HTTP requests to the MCP StreamableHTTPServer.
type MCPHandler struct {
	httpServer *mcpserver.StreamableHTTPServer
}

// NewMCPHandler creates a new MCP handler wired to the given services.
func NewMCPHandler(
	sandboxSvc *service.SandboxService,
	execSvc *service.ExecService,
	fsSvc *service.FSService,
	cmdsSvc *service.CommandsService,
) *MCPHandler {
	return &MCPHandler{
		httpServer: mcppkg.NewServer(sandboxSvc, execSvc, fsSvc, cmdsSvc),
	}
}

// Handle is a Gin handler that delegates to the MCP StreamableHTTPServer.
// It bridges auth info from Gin's context into the *http.Request context
// so that WithHTTPContextFunc can pick it up.
func (h *MCPHandler) Handle(c *gin.Context) {
	// Extract auth info set by middleware.AuthMiddleware via c.Set("orgID", ...)
	orgIDHex := c.GetString("orgID")
	userIDHex := c.GetString("userID")

	// Parse ObjectIDs and inject into request context
	ctx := c.Request.Context()

	if orgIDHex != "" {
		if orgID, err := util.ParseObjectID(orgIDHex); err == nil {
			ctx = context.WithValue(ctx, mcppkg.ReqOrgIDKey, orgID)
		}
	}
	if userIDHex != "" {
		if userID, err := util.ParseObjectID(userIDHex); err == nil {
			ctx = context.WithValue(ctx, mcppkg.ReqUserIDKey, userID)
		}
	}

	// Create a new request with the enriched context
	req := c.Request.WithContext(ctx)

	// Delegate to the MCP StreamableHTTPServer (implements http.Handler)
	h.httpServer.ServeHTTP(c.Writer, req)
}
