package handler

import (
	"net/http"
	"strings"

	"voidrun/config"
	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
)

// CommandsHandler handles process management HTTP requests
type CommandsHandler struct {
	commandsService *service.CommandsService
	sandboxService  *service.SandboxService
}

// NewCommandsHandler creates a new commands handler
func NewCommandsHandler(commandsService *service.CommandsService, sandboxService *service.SandboxService) *CommandsHandler {
	return &CommandsHandler{
		commandsService: commandsService,
		sandboxService:  sandboxService,
	}
}

// Run starts a background process
// POST /sandboxes/:id/commands/run
func (h *CommandsHandler) Run(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req model.CommandRunRequest
	if err := c.BindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		return util.ErrBadRequest("Command is required")
	}
	if len(req.Command) > config.MaxCommandLength {
		return util.ErrBadRequest("Command exceeds maximum length")
	}

	resp, err := h.commandsService.Run(id, req)
	if err != nil {
		return util.ErrInternal("Failed to run command", err)
	}

	c.JSON(http.StatusOK, resp)
	return nil
}

// List returns all running processes
// GET /sandboxes/:id/commands/list
func (h *CommandsHandler) List(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	resp, err := h.commandsService.List(id)
	if err != nil {
		return util.ErrInternal("Failed to list processes", err)
	}

	c.JSON(http.StatusOK, resp)
	return nil
}

// Kill terminates a process
// POST /sandboxes/:id/commands/kill
func (h *CommandsHandler) Kill(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req model.CommandKillRequest
	if err := c.BindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	if req.PID <= 0 {
		return util.ErrBadRequest("Invalid PID")
	}

	resp, err := h.commandsService.Kill(id, req.PID)
	if err != nil {
		return util.ErrBadRequest(err.Error())
	}

	c.JSON(http.StatusOK, resp)
	return nil
}

// Attach streams output from a running process
// POST /sandboxes/:id/commands/attach
func (h *CommandsHandler) Attach(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req model.CommandAttachRequest
	if err := c.BindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	if req.PID <= 0 {
		return util.ErrBadRequest("Invalid PID")
	}

	// Set SSE headers — from this point the response is "taken over";
	// any error from Attach is returned but WriteError will no-op since
	// the writer is already open.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	if err := h.commandsService.Attach(id, req.PID, c.Writer, func() { c.Writer.Flush() }); err != nil {
		return util.ErrInternal(err.Error(), nil)
	}
	return nil
}

// Wait waits for a process to complete
// POST /sandboxes/:id/commands/wait
func (h *CommandsHandler) Wait(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req model.CommandWaitRequest
	if err := c.BindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request")
	}

	if req.PID <= 0 {
		return util.ErrBadRequest("Invalid PID")
	}

	resp, err := h.commandsService.Wait(id, req.PID)
	if err != nil {
		return util.ErrInternal("Wait failed", err)
	}

	c.JSON(http.StatusOK, resp)
	return nil
}
