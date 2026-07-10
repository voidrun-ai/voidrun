package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"voidrun/config"
	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

	"github.com/mark3labs/mcp-go/mcp"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Handlers holds all service dependencies needed by MCP tool handlers.
type Handlers struct {
	SandboxService  *service.SandboxService
	ExecService     *service.ExecService
	FSService       *service.FSService
	CommandsService *service.CommandsService
}

// requireOrgID extracts the org ID from the MCP context. Returns an error result if missing.
func requireOrgID(ctx context.Context) (primitive.ObjectID, error) {
	orgID, ok := OrgIDFromContext(ctx)
	if !ok || orgID.IsZero() {
		return primitive.NilObjectID, fmt.Errorf("missing org context in MCP request")
	}
	return orgID, nil
}

// requireUserID extracts the user ID from the MCP context.
func requireUserID(ctx context.Context) (primitive.ObjectID, error) {
	userID, ok := UserIDFromContext(ctx)
	if !ok || userID.IsZero() {
		return primitive.NilObjectID, fmt.Errorf("missing user context in MCP request")
	}
	return userID, nil
}

// ensureRunning checks that the sandbox is running (auto-resumes if paused)
// and touches activity for auto-pause tracking.
func (h *Handlers) ensureRunning(ctx context.Context, orgID primitive.ObjectID, sandboxID string) error {
	if err := h.SandboxService.EnsureRunning(ctx, orgID, sandboxID); err != nil {
		return err
	}
	go h.SandboxService.TouchActivity(ctx, sandboxID)
	return nil
}

// readAgentJSON reads and returns the JSON body from an agent *http.Response.
// On agent-side errors (status >= 400), returns an error.
func readAgentJSON(resp *http.Response) (string, error) {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read agent response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("agent error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return string(body), nil
}

// readAgentText reads the raw body text from an agent *http.Response.
func readAgentText(resp *http.Response) (string, error) {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read agent response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("agent error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return string(body), nil
}

// toJSON marshals a value to a JSON string.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":"failed to marshal result: %s"}`, err.Error())
	}
	return string(b)
}

// requireString extracts a required string argument from the tool request.
func requireString(req mcp.CallToolRequest, key string) (string, error) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return "", fmt.Errorf("missing required argument: %s", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %s must be a string", key)
	}
	if s == "" {
		return "", fmt.Errorf("argument %s must not be empty", key)
	}
	return s, nil
}

// optionalString extracts an optional string argument, returning defaultVal if absent.
func optionalString(req mcp.CallToolRequest, key, defaultVal string) string {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return defaultVal
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return defaultVal
	}
	return s
}

// optionalNumber extracts an optional numeric argument, returning defaultVal if absent.
func optionalNumber(req mcp.CallToolRequest, key string, defaultVal int) int {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return defaultVal
		}
		return int(i)
	default:
		return defaultVal
	}
}

func optionalBoolPtr(req mcp.CallToolRequest, key string) *bool {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return nil
	}
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

func optionalIntSlice(req mcp.CallToolRequest, key string) ([]int, error) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return nil, nil
	}
	switch arr := v.(type) {
	case []int:
		out := make([]int, len(arr))
		copy(out, arr)
		return out, nil
	case []interface{}:
		out := make([]int, 0, len(arr))
		for i, elem := range arr {
			n, err := parseIntArg(elem)
			if err != nil {
				return nil, fmt.Errorf("argument %s[%d]: %w", key, i, err)
			}
			out = append(out, n)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("argument %s must be an array of integers", key)
	}
}

func parseIntArg(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}

func optionalStringMap(req mcp.CallToolRequest, key string) map[string]string {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return nil
	}
	raw, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// requiredNumber extracts a required numeric argument.
func requiredNumber(req mcp.CallToolRequest, key string) (int, error) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("missing required argument: %s", key)
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("argument %s must be a number", key)
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("argument %s must be a number", key)
	}
}

// --- Sandbox Management Handlers ---

// HandleCreateSandbox creates a new sandbox VM.
func (h *Handlers) HandleCreateSandbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	userID, err := requireUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	name, err := requireString(req, "name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	image := optionalString(req, "image", "code")
	cpu := optionalNumber(req, "cpu", 1)
	mem := optionalNumber(req, "mem", config.DefaultSandboxMemoryMB)

	sync := optionalBoolPtr(req, "sync")
	autoSleep := optionalBoolPtr(req, "autoSleep")
	envVars := optionalStringMap(req, "envVars")
	region := optionalString(req, "region", "")

	publishPorts, err := optionalIntSlice(req, "publishPorts")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := util.ValidateCreateSandboxRequest(name, cpu, mem, publishPorts); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	createReq := model.CreateSandboxRequest{
		Name:         name,
		Image:        image,
		CPU:          cpu,
		Mem:          mem,
		OrgID:        orgID,
		UserID:       userID,
		Sync:         sync,
		EnvVars:      envVars,
		AutoSleep:    autoSleep,
		Region:       region,
		PublishPorts: publishPorts,
	}

	sandbox, err := h.SandboxService.Create(ctx, createReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to create sandbox: %s", err.Error())), nil
	}

	return mcp.NewToolResultText(toJSON(sandbox)), nil
}

// HandleListSandboxes lists all sandboxes for the org.
func (h *Handlers) HandleListSandboxes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	page := optionalNumber(req, "page", 1)
	limit := optionalNumber(req, "limit", 0)

	sandboxes, total, _, err := h.SandboxService.ListByOrgPaginated(ctx, orgID, page, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list sandboxes: %s", err.Error())), nil
	}

	result := map[string]any{
		"sandboxes": sandboxes,
		"total":     total,
		"page":      page,
	}
	return mcp.NewToolResultText(toJSON(result)), nil
}

// HandleGetSandbox returns details of a specific sandbox.
func (h *Handlers) HandleGetSandbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	sandbox, err := h.SandboxService.Get(ctx, orgID, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get sandbox: %s", err.Error())), nil
	}

	return mcp.NewToolResultText(toJSON(sandbox)), nil
}

// HandleDeleteSandbox deletes a sandbox.
func (h *Handlers) HandleDeleteSandbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.SandboxService.Delete(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to delete sandbox: %s", err.Error())), nil
	}

	return mcp.NewToolResultText(`{"status":"deleted"}`), nil
}

// --- Command Execution Handlers ---

// HandleExecuteCommand executes a shell command synchronously in a sandbox.
func (h *Handlers) HandleExecuteCommand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	command, err := requireString(req, "command")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	timeout := optionalNumber(req, "timeout", 30)
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 300 {
		timeout = 300
	}

	cwd := optionalString(req, "cwd", "")

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.ExecService.ExecSync(ctx, id, command, timeout, nil, cwd)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Command execution failed: %s", err.Error())), nil
	}

	body, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(body), nil
}

// --- Filesystem Handlers ---

// HandleReadFile reads a file from a sandbox.
func (h *Handlers) HandleReadFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path, err := requireString(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.FSService.DownloadFile(ctx, id, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to read file: %s", err.Error())), nil
	}

	content, err := readAgentText(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(content), nil
}

// HandleWriteFile writes content to a file in a sandbox.
func (h *Handlers) HandleWriteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path, err := requireString(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	content, err := requireString(req, "content")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	body := strings.NewReader(content)
	contentLen := fmt.Sprintf("%d", len(content))

	resp, err := h.FSService.UploadFile(ctx, id, path, body, contentLen, "application/octet-stream")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to write file: %s", err.Error())), nil
	}

	result, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(result), nil
}

// HandleListFiles lists files in a sandbox directory.
func (h *Handlers) HandleListFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path := optionalString(req, "path", "/root")

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.FSService.ListFiles(ctx, id, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list files: %s", err.Error())), nil
	}

	body, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(body), nil
}

// HandleCreateDirectory creates a directory in a sandbox.
func (h *Handlers) HandleCreateDirectory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path, err := requireString(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.FSService.CreateDirectory(ctx, id, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to create directory: %s", err.Error())), nil
	}

	body, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(body), nil
}

// HandleDeleteFile deletes a file or directory from a sandbox.
func (h *Handlers) HandleDeleteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path, err := requireString(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.FSService.DeleteFile(ctx, id, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to delete file: %s", err.Error())), nil
	}

	body, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(body), nil
}

// HandleMoveFile moves/renames a file or directory within a sandbox.
func (h *Handlers) HandleMoveFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	from, err := requireString(req, "from")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	to, err := requireString(req, "to")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.FSService.MoveFile(ctx, id, from, to)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to move file: %s", err.Error())), nil
	}

	body, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(body), nil
}

// HandleFileInfo gets file metadata from a sandbox.
func (h *Handlers) HandleFileInfo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path, err := requireString(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.FSService.StatFile(ctx, id, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get file info: %s", err.Error())), nil
	}

	body, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(body), nil
}

// HandleSearchFiles searches for files in a sandbox.
func (h *Handlers) HandleSearchFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path := optionalString(req, "path", "/root")

	pattern, err := requireString(req, "pattern")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	resp, err := h.FSService.SearchFiles(ctx, id, path, pattern)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to search files: %s", err.Error())), nil
	}

	body, err := readAgentJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(body), nil
}

// --- Background Process Handlers ---

// HandleRunBackgroundCommand starts a background process in a sandbox.
func (h *Handlers) HandleRunBackgroundCommand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	command, err := requireString(req, "command")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	cwd := optionalString(req, "cwd", "")

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	runResp, err := h.CommandsService.Run(id, model.CommandRunRequest{
		Command: command,
		Cwd:     cwd,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to start background command: %s", err.Error())), nil
	}

	return mcp.NewToolResultText(toJSON(runResp)), nil
}

// HandleListProcesses lists all background processes in a sandbox.
func (h *Handlers) HandleListProcesses(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	listResp, err := h.CommandsService.List(id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list processes: %s", err.Error())), nil
	}

	return mcp.NewToolResultText(toJSON(listResp)), nil
}

// HandleKillProcess kills a background process in a sandbox.
func (h *Handlers) HandleKillProcess(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	id, err := requireString(req, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	pid, err := requiredNumber(req, "pid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := h.ensureRunning(ctx, orgID, id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Sandbox not running: %s", err.Error())), nil
	}

	killResp, err := h.CommandsService.Kill(id, pid)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to kill process: %s", err.Error())), nil
	}

	return mcp.NewToolResultText(toJSON(killResp)), nil
}
