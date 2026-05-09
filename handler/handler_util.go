package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// connTracker is the package-level connection tracker shared by all handlers.
// Set via SetConnTracker during server initialization.
var connTracker *service.ConnTracker

// SetConnTracker sets the package-level connection tracker.
func SetConnTracker(ct *service.ConnTracker) {
	connTracker = ct
}

const ctxKeyTrackedSandbox = "_tracked_sandbox_id"

// HandlerFunc is like gin.HandlerFunc but returns an error.
//
// Returning a non-nil *util.APIError (or any error) causes Handle() to write
// the appropriate JSON response automatically via util.WriteError.
//
// Returning nil means the handler has already written its own response — or
// intentionally wrote nothing (which is valid for WebSocket / SSE handlers
// that take ownership of the connection).
type HandlerFunc func(*gin.Context) error

// Handle wraps an error-returning HandlerFunc into a standard gin.HandlerFunc.
// It is the only place in the codebase that translates an error into an HTTP
// response, ensuring consistent error shape across all endpoints.
// It also releases the ConnTracker connection when the handler returns.
func Handle(fn HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := fn(c)

		// Auto-release tracked connection when handler completes
		if sandboxID, ok := c.Get(ctxKeyTrackedSandbox); ok {
			if id, isStr := sandboxID.(string); isStr && connTracker != nil {
				connTracker.Release(id)
			}
		}

		if err != nil {
			util.WriteError(c, err)
			c.Abort()
		}
	}
}

// ensureSandboxRunning validates the org auth context, checks the sandbox is
// running, and fires a background TouchActivity call.
func ensureSandboxRunning(
	c *gin.Context,
	sandboxSvc *service.SandboxService,
	sandboxID string,
) error {
	_, err := ensureSandboxRunningWithOrg(c, sandboxSvc, sandboxID)
	return err
}

// ensureSandboxRunningWithOrg is the same as ensureSandboxRunning but also
// returns the resolved orgID for callers that need it.
func ensureSandboxRunningWithOrg(
	c *gin.Context,
	sandboxSvc *service.SandboxService,
	sandboxID string,
) (primitive.ObjectID, error) {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return primitive.NilObjectID, err
	}

	if err = sandboxSvc.EnsureRunning(c.Request.Context(), orgID, sandboxID); err != nil {
		return primitive.NilObjectID, util.ErrNotFound(err.Error())
	}

	// Track active connection for this sandbox
	if connTracker != nil {
		connTracker.Acquire(sandboxID)
		c.Set(ctxKeyTrackedSandbox, sandboxID)
	}

	// Touch activity for auto-pause tracking (async, fire-and-forget)
	go sandboxSvc.TouchActivity(c.Request.Context(), orgID, sandboxID)

	return orgID, nil
}

// HandleJSONResponse proxies the agent HTTP response back to the client in our
// standard envelope format.
func HandleJSONResponse(c *gin.Context, resp *http.Response) error {
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return util.ErrBadGateway("Failed to read sandbox response", err)
	}

	contentType := resp.Header.Get("Content-Type")
	status := resp.StatusCode

	if strings.Contains(contentType, "application/json") {
		raw := json.RawMessage(bodyBytes)
		if status >= 400 {
			return &util.APIError{Status: status, Message: "Sandbox error", Detail: string(bodyBytes)}
		}
		c.JSON(status, model.NewSuccessResponse("ok", raw))
		return nil
	}

	bodyStr := strings.TrimSpace(string(bodyBytes))
	if status >= 400 {
		return &util.APIError{Status: status, Message: "Sandbox error", Detail: bodyStr}
	}
	c.JSON(status, model.NewSuccessResponse(bodyStr, nil))
	return nil
}
