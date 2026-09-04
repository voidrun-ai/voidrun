package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
)

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
func Handle(fn HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := fn(c); err != nil {
			util.WriteError(c, err)
			c.Abort()
		}
	}
}

func admissionDenied(err error) error {
	if errors.Is(err, service.ErrAdmissionDenied) {
		return util.ErrOverloaded("scheduler unavailable")
	}
	return nil
}

func ensureSandboxRunning(
	c *gin.Context,
	sandboxSvc *service.SandboxService,
	sandboxID string,
) error {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	if err = sandboxSvc.EnsureRunning(c.Request.Context(), orgID, sandboxID); err != nil {
		if d := admissionDenied(err); d != nil {
			return d
		}
		return util.ErrNotFound(err.Error())
	}

	// Touch activity for auto-pause tracking (async, fire-and-forget)
	go sandboxSvc.TouchActivity(c.Request.Context(), sandboxID)

	return nil
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
