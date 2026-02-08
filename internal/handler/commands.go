package handler

import (
	"net/http"
	"strings"

	"voidrun/internal/model"
	"voidrun/internal/service"

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
func (h *CommandsHandler) Run(c *gin.Context) {
	id := c.Param("id")

	// Resolve sandbox to VM instance name
	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	vmInstance := sandbox.ID.Hex()

	var req model.CommandRunRequest
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

	resp, err := h.commandsService.Run(vmInstance, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to run command", err.Error()))
		return
	}

	c.JSON(http.StatusOK, resp)
}

// List returns all running processes
// GET /sandboxes/:id/commands/list
func (h *CommandsHandler) List(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	vmInstance := sandbox.ID.Hex()

	resp, err := h.commandsService.List(vmInstance)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to list processes", err.Error()))
		return
	}

	c.JSON(http.StatusOK, resp)
}

// Kill terminates a process
// POST /sandboxes/:id/commands/kill
func (h *CommandsHandler) Kill(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	vmInstance := sandbox.ID.Hex()

	var req model.CommandKillRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	if req.PID <= 0 {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid PID", ""))
		return
	}

	resp, err := h.commandsService.Kill(vmInstance, req.PID)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, resp)
}

// Attach streams output from a running process
// POST /sandboxes/:id/commands/attach
func (h *CommandsHandler) Attach(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	vmInstance := sandbox.ID.Hex()

	var req model.CommandAttachRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	if req.PID <= 0 {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid PID", ""))
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	if err := h.commandsService.Attach(vmInstance, req.PID, c.Writer, func() { c.Writer.Flush() }); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
}

// Wait waits for a process to complete
// POST /sandboxes/:id/commands/wait
func (h *CommandsHandler) Wait(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	vmInstance := sandbox.ID.Hex()

	var req model.CommandWaitRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	if req.PID <= 0 {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid PID", ""))
		return
	}

	resp, err := h.commandsService.Wait(vmInstance, req.PID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, resp)
}
