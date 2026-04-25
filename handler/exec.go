package handler

import (
	"net/http"
	"strings"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
)

const (
	maxCommandLength = 100000
	maxSessionIDLen  = 100
)

// ExecHandler handles command execution HTTP requests
type ExecHandler struct {
	execService    *service.ExecService
	sessionService *service.SessionExecService
	sandboxService *service.SandboxService
}

// NewExecHandler creates a new exec handler
func NewExecHandler(execService *service.ExecService, sessionService *service.SessionExecService, sandboxService *service.SandboxService) *ExecHandler {
	return &ExecHandler{
		execService:    execService,
		sessionService: sessionService,
		sandboxService: sandboxService,
	}
}

// Exec handles POST /sandboxes/:id/exec
func (h *ExecHandler) Exec(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req model.ExecRequest
	if err := c.BindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		return util.ErrBadRequest("Command is required")
	}
	if len(req.Command) > maxCommandLength {
		return util.ErrBadRequest("Command exceeds maximum length")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 300 {
		timeout = 300
	}

	resp, err := h.execService.ExecSync(c.Request.Context(), id, req.Command, timeout, req.Env, req.Cwd)
	if err != nil {
		return util.ErrInternal("Command execution failed", err)
	}

	HandleJSONResponse(c, resp)
	return nil
}

// SessionExec handles POST /sandboxes/:id/session-exec
func (h *ExecHandler) SessionExec(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req model.SessionExecRequest
	if err := c.BindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	agentResp, err := h.sessionService.Send(id, req)
	if err != nil {
		if agentResp == nil {
			return util.ErrInternal(err.Error(), nil)
		}
		return util.ErrBadRequest(err.Error())
	}

	c.JSON(http.StatusOK, agentResp)
	return nil
}

// SessionExecStream handles POST /sandboxes/:id/session-exec-stream (streaming)
func (h *ExecHandler) SessionExecStream(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var payload struct {
		SessionID string `json:"sessionId"`
		Command   string `json:"command"`
	}
	if err := c.BindJSON(&payload); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	payload.SessionID = strings.TrimSpace(payload.SessionID)
	if payload.SessionID == "" {
		return util.ErrBadRequest("Session ID is required")
	}
	if len(payload.SessionID) > maxSessionIDLen {
		return util.ErrBadRequest("Session ID exceeds maximum length")
	}
	payload.Command = strings.TrimSpace(payload.Command)
	if payload.Command == "" {
		return util.ErrBadRequest("Command is required")
	}
	if len(payload.Command) > maxCommandLength {
		return util.ErrBadRequest("Command exceeds maximum length")
	}

	// Set streaming headers — from this point WriteError will no-op
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Transfer-Encoding", "chunked")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-cache")

	if err := h.sessionService.StreamExec(id, payload.SessionID, payload.Command, c.Writer, func() { c.Writer.Flush() }); err != nil {
		return util.ErrInternal(err.Error(), nil)
	}
	return nil
}

// ExecStream handles POST /sandboxes/:id/exec-stream for streaming command output as SSE
func (h *ExecHandler) ExecStream(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req model.ExecRequest
	if err := c.BindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		return util.ErrBadRequest("Command is required")
	}
	if len(req.Command) > maxCommandLength {
		return util.ErrBadRequest("Command exceeds maximum length")
	}

	_, _, timeout, err := h.execService.ParseAndValidateRequest(req)
	if err != nil {
		return util.ErrBadRequest(err.Error())
	}

	// Set SSE headers — from this point WriteError will no-op
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	if err := h.execService.ExecStreamSSE(c.Request.Context(), id, req.Command, timeout, req.Env, req.Cwd, c.Writer, func() { c.Writer.Flush() }); err != nil {
		return util.ErrInternal(err.Error(), nil)
	}
	return nil
}
