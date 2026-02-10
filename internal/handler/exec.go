package handler

import (
	"net/http"
	"strings"

	"voidrun/internal/model"
	"voidrun/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	maxCommandLength = 100000
	maxSessionIDLen  = 100
)

// ExecHandler handles command execution HTTP requests
type ExecHandler struct {
	execService     *service.ExecService
	sessionService  *service.SessionExecService
	sandboxService  *service.SandboxService
	commandsService *service.CommandsService
}

// NewExecHandler creates a new exec handler
func NewExecHandler(execService *service.ExecService, sessionService *service.SessionExecService, sandboxService *service.SandboxService, commandsService *service.CommandsService) *ExecHandler {
	return &ExecHandler{
		execService:     execService,
		sessionService:  sessionService,
		sandboxService:  sandboxService,
		commandsService: commandsService,
	}
}

// Exec handles POST /sandboxes/:id/exec
func (h *ExecHandler) Exec(c *gin.Context) {
	id := c.Param("id")

	// Get sandbox from database to retrieve instance name
	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	var req model.ExecRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	// Validate command
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Command is required", ""))
		return
	}
	if len(req.Command) > maxCommandLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Command exceeds maximum length", ""))
		return
	}

	// If background flag is set, delegate to commands service
	if req.Background {
		runResp, err := h.commandsService.Run(sbxInstance, model.CommandRunRequest{
			Command: req.Command,
			Env:     req.Env,
			Cwd:     req.Cwd,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to start background process", err.Error()))
			return
		}
		// Return wrapped response to match SDK expectations
		c.JSON(http.StatusOK, gin.H{
			"status":  "success",
			"message": "ok",
			"data":    runResp,
		})
		return
	}

	// Validate timeout
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 // Default 30 seconds
	}
	if timeout > 300 {
		timeout = 300 // Max 5 minutes
	}

	// Execute command synchronously via agent /exec endpoint
	resp, err := h.execService.ExecSync(c.Request.Context(), sbxInstance, req.Command, timeout, req.Env, req.Cwd)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Command execution failed", err.Error()))
		return
	}

	HandleJSONResponse(c, resp)
}

// SessionExec handles POST /sandboxes/:id/session-exec
func (h *ExecHandler) SessionExec(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	var req model.SessionExecRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	agentResp, err := h.sessionService.Send(sbxInstance, req)
	if err != nil {
		status := http.StatusBadRequest
		if agentResp == nil {
			status = http.StatusInternalServerError
		}
		c.JSON(status, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, agentResp)
}

// SessionExecStream handles POST /sandboxes/:id/session-exec-stream (streaming)
func (h *ExecHandler) SessionExecStream(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	var payload struct {
		SessionID string `json:"sessionId"`
		Command   string `json:"command"`
	}
	if err := c.BindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	// Validate session ID and command
	payload.SessionID = strings.TrimSpace(payload.SessionID)
	if payload.SessionID == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Session ID is required", ""))
		return
	}
	if len(payload.SessionID) > maxSessionIDLen {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Session ID exceeds maximum length", ""))
		return
	}
	payload.Command = strings.TrimSpace(payload.Command)
	if payload.Command == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Command is required", ""))
		return
	}
	if len(payload.Command) > maxCommandLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Command exceeds maximum length", ""))
		return
	}

	// Set streaming headers
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Transfer-Encoding", "chunked")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-cache")

	if err := h.sessionService.StreamExec(sbxInstance, payload.SessionID, payload.Command, c.Writer, func() { c.Writer.Flush() }); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
}

// ExecStream handles POST /sandboxes/:id/exec-stream for streaming command output as SSE
// @Summary Stream command execution (SSE)
// @Description Stream command execution output as Server-Sent Events
// @Tags exec
// @Accept json
// @Produce text/event-stream
// @Security ApiKeyAuth
// @Param id path string true "Sandbox ID"
// @Param request body model.ExecRequest true "Execution Request"
// @Success 200 {string} string "Stream output"
// @Failure 400 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /sandboxes/{id}/exec-stream [post]
func (h *ExecHandler) ExecStream(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}
	sbxInstance := sandbox.ID.Hex()

	var req model.ExecRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	// Validate command
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Command is required", ""))
		return
	}
	if len(req.Command) > maxCommandLength {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Command exceeds maximum length", ""))
		return
	}

	// Parse and validate request
	_, _, timeout, err := h.execService.ParseAndValidateRequest(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	if err := h.execService.ExecStreamSSE(c.Request.Context(), sbxInstance, req.Command, timeout, req.Env, req.Cwd, c.Writer, func() { c.Writer.Flush() }); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
}
