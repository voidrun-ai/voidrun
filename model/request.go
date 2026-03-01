package model

// CreateSandboxRequest represents the request to create a new sandbox
type CreateSandboxRequest struct {
	Name       string            `json:"name" binding:"required"`
	TemplateID string            `json:"templateId,omitempty"`
	CPU        int               `json:"cpu" binding:"min=1,max=8"`
	Mem        int               `json:"mem" binding:"min=1024,max=16384"`
	OrgID      string            `json:"orgId,omitempty"`
	UserID     string            `json:"userId,omitempty"`
	Sync       *bool             `json:"sync"`
	EnvVars    map[string]string `json:"envVars,omitempty"`
}

// ExecRequest represents a command execution request
type ExecRequest struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Timeout    int               `json:"timeout"`
	Env        map[string]string `json:"env,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	Background bool              `json:"background,omitempty"` // If true, starts as background process and returns PID
}

// SessionExecRequest represents a PTY session action forwarded to the agent
type SessionExecRequest struct {
	Action    string `json:"action" binding:"required"` // create, exec, input, resize, close
	SessionID string `json:"sessionId"`                 // required for all but create; auto-generated when empty on create
	Shell     string `json:"shell"`                     // optional shell for create
	Command   string `json:"command"`                   // required for exec
	Input     string `json:"input"`                     // optional input for input
	Cols      uint16 `json:"cols"`                      // required for resize (and optional default for create)
	Rows      uint16 `json:"rows"`
}

// CommandRunRequest represents a background process run request
type CommandRunRequest struct {
	Command string            `json:"command" binding:"required"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout int               `json:"timeout,omitempty"` // Timeout in seconds, 0 = no timeout
}

// CommandKillRequest represents a process kill request
type CommandKillRequest struct {
	PID int `json:"pid" binding:"required,min=1"`
}

// CommandAttachRequest represents a request to attach to a running process
type CommandAttachRequest struct {
	PID int `json:"pid" binding:"required,min=1"`
}

// CommandWaitRequest represents a request to wait for a process
type CommandWaitRequest struct {
	PID int `json:"pid" binding:"required,min=1"`
}

type RegisterRequest struct {
	Name  string `json:"name" binding:"required,min=2"`
	Email string `json:"email" binding:"required,email"`
}

// GenerateAPIKeyRequest represents a request to generate a new API key
type GenerateAPIKeyRequest struct {
	OrgID   string `json:"orgId" binding:"required"`
	KeyName string `json:"keyName" binding:"required"` // Human-readable name
}

// RevokeAPIKeyRequest represents a request to revoke/delete an API key
type RevokeAPIKeyRequest struct {
	OrgID string `json:"orgId" binding:"required"`
	KeyID string `json:"keyId" binding:"required"`
}

// ListAPIKeysRequest represents a request to list all API keys for an org
type ListAPIKeysRequest struct {
	OrgID string `json:"orgId" binding:"required"`
}

// ActivateAPIKeyRequest represents a request to activate/deactivate an API key
type ActivateAPIKeyRequest struct {
	OrgID    string `json:"orgId" binding:"required"`
	KeyID    string `json:"keyId" binding:"required"`
	IsActive bool   `json:"isActive"`
}
