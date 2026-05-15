package mcp

import (
	"context"
	"net/http"

	"voidrun/service"
	"voidrun/util"

	"github.com/mark3labs/mcp-go/server"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// NewServer creates a configured MCP StreamableHTTPServer with all VoidRun tools registered.
// The returned server implements http.Handler and can be mounted on any HTTP mux.
func NewServer(
	sandboxSvc *service.SandboxService,
	execSvc *service.ExecService,
	fsSvc *service.FSService,
	cmdsSvc *service.CommandsService,
	imageSvc *service.ImageService,
) *server.StreamableHTTPServer {
	ver := util.Get()

	mcpServer := server.NewMCPServer(
		"voidrun",
		ver.Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(false, true),
		server.WithInstructions("VoidRun MCP Server — manage cloud sandbox VMs, execute commands, and manipulate files inside sandboxes."),
	)

	h := &Handlers{
		SandboxService:  sandboxSvc,
		ExecService:     execSvc,
		FSService:       fsSvc,
		CommandsService: cmdsSvc,
	}

	// Register all tools with their handlers
	mcpServer.AddTool(toolCreateSandbox(), h.HandleCreateSandbox)
	mcpServer.AddTool(toolListSandboxes(), h.HandleListSandboxes)
	mcpServer.AddTool(toolGetSandbox(), h.HandleGetSandbox)
	mcpServer.AddTool(toolDeleteSandbox(), h.HandleDeleteSandbox)
	mcpServer.AddTool(toolExecuteCommand(), h.HandleExecuteCommand)
	mcpServer.AddTool(toolReadFile(), h.HandleReadFile)
	mcpServer.AddTool(toolWriteFile(), h.HandleWriteFile)
	mcpServer.AddTool(toolListFiles(), h.HandleListFiles)
	mcpServer.AddTool(toolCreateDirectory(), h.HandleCreateDirectory)
	mcpServer.AddTool(toolDeleteFile(), h.HandleDeleteFile)
	mcpServer.AddTool(toolMoveFile(), h.HandleMoveFile)
	mcpServer.AddTool(toolFileInfo(), h.HandleFileInfo)
	mcpServer.AddTool(toolSearchFiles(), h.HandleSearchFiles)
	mcpServer.AddTool(toolRunBackgroundCommand(), h.HandleRunBackgroundCommand)
	mcpServer.AddTool(toolListProcesses(), h.HandleListProcesses)
	mcpServer.AddTool(toolKillProcess(), h.HandleKillProcess)

	// Register MCP resources — static and templated
	rh := &resourceHandlers{
		sandboxSvc: sandboxSvc,
		imageSvc:   imageSvc,
		fsSvc:      fsSvc,
	}
	mcpServer.AddResource(resourceSandboxesList(), rh.handleSandboxesList)
	mcpServer.AddResource(resourceImagesList(), rh.handleImagesList)
	mcpServer.AddResourceTemplate(templateSandboxStatus(), rh.handleSandboxStatus)
	mcpServer.AddResourceTemplate(templateSandboxFiles(), rh.handleSandboxFile)

	// Create the StreamableHTTP server with context bridging
	httpServer := server.NewStreamableHTTPServer(mcpServer,
		server.WithHTTPContextFunc(contextBridgeFunc),
	)

	return httpServer
}

// contextBridgeFunc reads orgID and userID injected into the request context
// by the Gin auth middleware bridge (handler/mcp.go) and stores them as typed
// values the MCP tool handlers can retrieve via OrgIDFromContext / UserIDFromContext.
func contextBridgeFunc(ctx context.Context, r *http.Request) context.Context {
	if orgID, ok := r.Context().Value(ReqOrgIDKey).(primitive.ObjectID); ok {
		ctx = WithOrgID(ctx, orgID)
	}
	if userID, ok := r.Context().Value(ReqUserIDKey).(primitive.ObjectID); ok {
		ctx = WithUserID(ctx, userID)
	}
	return ctx
}

// Request-level context keys used by handler/mcp.go to pass auth info
// from Gin context into the standard *http.Request context.
type ReqContextKey string

const (
	ReqOrgIDKey  ReqContextKey = "req_orgID"
	ReqUserIDKey ReqContextKey = "req_userID"
)
