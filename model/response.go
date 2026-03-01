package model

import "time"

// Response is a generic API response
type Response struct {
	Status  string                 `json:"status"`
	Message string                 `json:"message"`
	Data    any                    `json:"data,omitempty"`
	Meta    map[string]interface{} `json:"meta,omitempty"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// NewSuccessResponse creates a success response
func NewSuccessResponse(message string, data any) Response {
	return Response{
		Status:  "success",
		Message: message,
		Data:    data,
	}
}

// NewSuccessResponseWithMeta creates a success response with metadata (for pagination, etc.)
func NewSuccessResponseWithMeta(message string, data any, meta map[string]interface{}) Response {
	return Response{
		Status:  "success",
		Message: message,
		Data:    data,
		Meta:    meta,
	}
}

// NewErrorResponse creates an error response
func NewErrorResponse(err string, details string) ErrorResponse {
	return ErrorResponse{
		Error:   err,
		Details: details,
	}
}

// GeneratedAPIKeyResponse represents the response when generating a new API key
// The PlainKey is only returned once at generation time
type GeneratedAPIKeyResponse struct {
	PlainKey  string    `json:"plainKey"` // Only returned once, user must store securely
	KeyID     string    `json:"keyId"`
	KeyName   string    `json:"keyName"`
	OrgID     string    `json:"orgId"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresIn string    `json:"expiresIn"` // Info message about key validity
}

// SessionExecResponse mirrors the agent's PTY session response payload
type SessionExecResponse struct {
	Success   bool   `json:"success"`
	SessionID string `json:"sessionId,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	ExitCode  int    `json:"exitCode,omitempty"`
}

// ExecResponse represents the response from agent /exec endpoint
type ExecResponse struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
}

// ProcessInfo represents a running process
type ProcessInfo struct {
	PID       int       `json:"pid"`
	Command   string    `json:"command"`
	StartTime time.Time `json:"startTime"`
	Running   bool      `json:"running"`
	ExitCode  *int      `json:"exitCode,omitempty"`
}

// CommandRunResponse represents the response from running a background process
type CommandRunResponse struct {
	Success bool   `json:"success"`
	PID     int    `json:"pid"`
	Command string `json:"command"`
}

// CommandListResponse represents the response from listing processes
type CommandListResponse struct {
	Success   bool          `json:"success"`
	Processes []ProcessInfo `json:"processes"`
}

// CommandKillResponse represents the response from killing a process
type CommandKillResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// CommandWaitResponse represents the response from waiting for a process
type CommandWaitResponse struct {
	Success  bool   `json:"success"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// OrgResponse represents a single organization in API responses
type OrgResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Plan      string    `json:"plan"`
	Usage     int       `json:"usage"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CurrentOrgResponse represents the response for GetCurrentOrg endpoint
type CurrentOrgResponse struct {
	OrgResponse
	ActiveOrgID string        `json:"activeOrgId"`
	Orgs        []OrgResponse `json:"orgs"`
}
