package handler

import (
	"errors"
	"net/http"
	"strings"

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
func (h *CommandsHandler) Run(c *gin.Context) {
	id := c.Param("id")

	if err := h.isSandboxRunning(c, id); err != nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse(err.Error(), ""))
		return
	}

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

	resp, err := h.commandsService.Run(id, req)
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

	if err := h.isSandboxRunning(c, id); err != nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse(err.Error(), ""))
		return
	}

	resp, err := h.commandsService.List(id)
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

	if err := h.isSandboxRunning(c, id); err != nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse(err.Error(), ""))
		return
	}

	var req model.CommandKillRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	if req.PID <= 0 {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid PID", ""))
		return
	}

	resp, err := h.commandsService.Kill(id, req.PID)
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

	if err := h.isSandboxRunning(c, id); err != nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse(err.Error(), ""))
		return
	}

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

	if err := h.commandsService.Attach(id, req.PID, c.Writer, func() { c.Writer.Flush() }); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}
}

// Wait waits for a process to complete
// POST /sandboxes/:id/commands/wait
func (h *CommandsHandler) Wait(c *gin.Context) {
	id := c.Param("id")

	if err := h.isSandboxRunning(c, id); err != nil {
		c.JSON(http.StatusNotFound, model.NewErrorResponse(err.Error(), ""))
		return
	}

	var req model.CommandWaitRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", ""))
		return
	}

	if req.PID <= 0 {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid PID", ""))
		return
	}

	resp, err := h.commandsService.Wait(id, req.PID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse(err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, resp)
}

func (h *CommandsHandler) isSandboxRunning(c *gin.Context, sandboxId string) error {
	orgID, err := util.GetOrgIDFromContext(c)
	if err != nil {
		return err
	}

	isRunning, err := h.sandboxService.IsRunning(c.Request.Context(), orgID, sandboxId)
	if err != nil {
		return err
	}

	if !isRunning {
		return errors.New("Sandbox not running")
	}
	return nil
}
