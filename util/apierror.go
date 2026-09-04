package util

import (
	"errors"
	"net/http"

	"voidrun/model"

	"github.com/gin-gonic/gin"
)

// APIError is an HTTP-aware error that carries the status code and
// a user-facing message. Handlers return it; Handle() dispatches
// it to WriteError which writes exactly one JSON response.
type APIError struct {
	Status  int
	Message string
	Detail  string
}

func (e *APIError) Error() string { return e.Message }

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

// ErrBadRequest returns a 400 error. Optional second arg sets the detail field.
func ErrBadRequest(msg string, detail ...string) *APIError {
	d := ""
	if len(detail) > 0 {
		d = detail[0]
	}
	return &APIError{http.StatusBadRequest, msg, d}
}

// ErrNotFound returns a 404 error.
func ErrNotFound(msg string) *APIError {
	return &APIError{http.StatusNotFound, msg, ""}
}

// ErrInternal returns a 500 error. cause.Error() is placed in the detail field.
func ErrInternal(msg string, cause error) *APIError {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return &APIError{http.StatusInternalServerError, msg, detail}
}

// ErrGatewayTimeout returns a 504 error (handler deadline exceeded).
func ErrGatewayTimeout(msg string, cause error) *APIError {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return &APIError{http.StatusGatewayTimeout, msg, detail}
}

// ErrBadGateway returns a 502 error (upstream / agent unreachable).
func ErrBadGateway(msg string, cause error) *APIError {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return &APIError{http.StatusBadGateway, msg, detail}
}

// ErrForbidden returns a 403 error.
func ErrForbidden(msg string) *APIError {
	return &APIError{http.StatusForbidden, msg, ""}
}

// ErrConflict returns a 409 error.
func ErrConflict(msg string) *APIError {
	return &APIError{http.StatusConflict, msg, ""}
}

// ErrTooLarge returns a 413 error.
func ErrTooLarge(msg string) *APIError {
	return &APIError{http.StatusRequestEntityTooLarge, msg, ""}
}

// ErrUnauthorized returns a 401 error.
func ErrUnauthorized(msg string) *APIError {
	return &APIError{http.StatusUnauthorized, msg, ""}
}

// ErrServiceUnavailable returns a 503 error.
func ErrServiceUnavailable(msg string) *APIError {
	return &APIError{http.StatusServiceUnavailable, msg, ""}
}

// StatusOverloaded is a non-standard 5xx (Cloudflare-style custom code).
const StatusOverloaded = 529

// ErrOverloaded returns a 529 error.
func ErrOverloaded(msg string) *APIError {
	return &APIError{StatusOverloaded, msg, ""}
}

// ---------------------------------------------------------------------------
// WriteError
// ---------------------------------------------------------------------------

// WriteError writes one JSON error response derived from err.
//
// Rules:
//   - If err is nil, this is a no-op.
//   - If the response is already written (streaming / WebSocket upgrade), this
//     is a no-op — safe to call unconditionally from Handle().
//   - *APIError values produce a response with their specific status code.
//   - Any other error produces a 500.
func WriteError(c *gin.Context, err error) {
	if err == nil || c.Writer.Written() {
		return
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		c.JSON(apiErr.Status, model.NewErrorResponse(apiErr.Message, apiErr.Detail))
		return
	}
	c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
}
