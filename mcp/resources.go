package mcp

import (
	"context"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"regexp"

	"voidrun/service"

	"github.com/mark3labs/mcp-go/mcp"
)

// resourceHandlers holds services needed by MCP resource handlers.
type resourceHandlers struct {
	sandboxSvc *service.SandboxService
	imageSvc   *service.ImageService
	fsSvc      *service.FSService
}

// --- Static resources ---

// resourceSandboxesList is a static resource listing all sandboxes for the org.
func resourceSandboxesList() mcp.Resource {
	return mcp.Resource{
		URI:         "voidrun://sandboxes",
		Name:        "Sandboxes",
		Description: "List of all sandboxes in the organization with their current status and resource allocation.",
		MIMEType:    "application/json",
	}
}

// resourceImagesList is a static resource listing available base images.
func resourceImagesList() mcp.Resource {
	return mcp.Resource{
		URI:         "voidrun://images",
		Name:        "Available Images",
		Description: "Base images available when creating a sandbox (e.g. debian, alpine).",
		MIMEType:    "application/json",
	}
}

// --- Resource templates ---

// templateSandboxStatus matches voidrun://sandbox/<id>/status
func templateSandboxStatus() mcp.ResourceTemplate {
	return mcp.ResourceTemplate{
		URITemplate: mustURITemplate("voidrun://sandbox/{id}/status"),
		Name:        "Sandbox Status",
		Description: "Details and current status for a specific sandbox. Replace {id} with the sandbox ID.",
		MIMEType:    "application/json",
	}
}

// templateSandboxFiles matches voidrun://sandbox/<id>/files{/path*}
func templateSandboxFiles() mcp.ResourceTemplate {
	return mcp.ResourceTemplate{
		URITemplate: mustURITemplate("voidrun://sandbox/{id}/files{/path*}"),
		Name:        "Sandbox File",
		Description: "Read a file from inside a sandbox. Replace {id} with sandbox ID and {/path*} with the absolute file path (e.g. /root/app.ts).",
		MIMEType:    "text/plain",
	}
}

// mustURITemplate parses a URI template string. Panics on invalid input (startup only).
func mustURITemplate(t string) *mcp.URITemplate {
	u := &mcp.URITemplate{}
	if err := u.UnmarshalJSON([]byte(`"` + t + `"`)); err != nil {
		panic(fmt.Sprintf("mcp: invalid URI template %q: %v", t, err))
	}
	return u
}

// --- Handler: voidrun://sandboxes ---

func (h *resourceHandlers) handleSandboxesList(
	ctx context.Context,
	req mcp.ReadResourceRequest,
) ([]mcp.ResourceContents, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return nil, err
	}

	sandboxes, total, _, err := h.sandboxSvc.ListByOrgPaginated(ctx, orgID, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}

	result := map[string]any{
		"total":     total,
		"sandboxes": sandboxes,
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     toJSON(result),
		},
	}, nil
}

// --- Handler: voidrun://images ---

func (h *resourceHandlers) handleImagesList(
	ctx context.Context,
	req mcp.ReadResourceRequest,
) ([]mcp.ResourceContents, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return nil, err
	}

	images, err := h.imageSvc.ListByOrg(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list images: %w", err)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     toJSON(images),
		},
	}, nil
}

// --- Handler: voidrun://sandbox/{id}/status ---

var reSandboxStatusURI = regexp.MustCompile(`^voidrun://sandbox/([^/]+)/status$`)

func (h *resourceHandlers) handleSandboxStatus(
	ctx context.Context,
	req mcp.ReadResourceRequest,
) ([]mcp.ResourceContents, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return nil, err
	}

	m := reSandboxStatusURI.FindStringSubmatch(req.Params.URI)
	if m == nil {
		return nil, fmt.Errorf("invalid sandbox status URI: %s", req.Params.URI)
	}
	id := m[1]

	sandbox, err := h.sandboxSvc.Get(ctx, orgID, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox: %w", err)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     toJSON(sandbox),
		},
	}, nil
}

// --- Handler: voidrun://sandbox/{id}/files{/path*} ---

var reSandboxFilesURI = regexp.MustCompile(`^voidrun://sandbox/([^/]+)/files(/.*)?$`)

func (h *resourceHandlers) handleSandboxFile(
	ctx context.Context,
	req mcp.ReadResourceRequest,
) ([]mcp.ResourceContents, error) {
	orgID, err := requireOrgID(ctx)
	if err != nil {
		return nil, err
	}

	m := reSandboxFilesURI.FindStringSubmatch(req.Params.URI)
	if m == nil {
		return nil, fmt.Errorf("invalid sandbox file URI: %s", req.Params.URI)
	}
	id := m[1]
	path := m[2]
	if path == "" {
		path = "/root"
	}

	if err := h.sandboxSvc.EnsureRunning(ctx, orgID, id); err != nil {
		return nil, fmt.Errorf("sandbox not running: %w", err)
	}

	resp, err := h.fsSvc.DownloadFile(ctx, id, path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent error reading file (status %d): %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read file body: %w", err)
	}

	// Pick a reasonable MIME type from extension; fall back to text/plain.
	mimeType := "text/plain"
	if ext := filepath.Ext(path); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			mimeType = mt
		}
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: mimeType,
			Text:     string(body),
		},
	}, nil
}
