package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"voidrun/internal/model"
	"voidrun/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// PTYHandler proxies WebSocket traffic between browser and agent /pty via vsock.
type PTYHandler struct {
	dialer         *service.VsockWSDialer
	sessionService *service.PTYSessionService
	sandboxService *service.SandboxService
}

func NewPTYHandler(dialer *service.VsockWSDialer, sessionService *service.PTYSessionService, sandboxService *service.SandboxService) *PTYHandler {
	return &PTYHandler{
		dialer:         dialer,
		sessionService: sessionService,
		sandboxService: sandboxService,
	}
}

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// Proxy handles the ephemeral PTY WebSocket connection (existing functionality)
func (h *PTYHandler) Proxy(c *gin.Context) {
	sbxInstance := c.Param("id")

	clientConn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	agentConn, _, err := h.dialer.DialContext(ctx, "ws://"+sbxInstance+"/pty", nil)
	if err != nil {
		return
	}
	defer agentConn.Close()

	// Use WaitGroup to ensure both goroutines complete before closing
	var wg sync.WaitGroup

	// Create a cancellation channel for graceful shutdown
	shutdownChan := make(chan struct{})
	var shutdownOnce sync.Once
	closeShutdown := func() {
		shutdownOnce.Do(func() {
			close(shutdownChan)
		})
	}

	// Client -> Agent (Input)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-shutdownChan:
				return
			default:
			}

			clientConn.SetReadDeadline(time.Time{})
			mt, msg, err := clientConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
					// Log unexpected close errors
					_ = err
				}
				closeShutdown() // Signal other goroutine to exit
				return
			}

			// Send to agent with timeout
			agentConn.SetWriteDeadline(time.Time{})
			if err = agentConn.WriteMessage(mt, msg); err != nil {
				closeShutdown()
				return
			}
		}
	}()

	// Agent -> Client (Output)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-shutdownChan:
				return
			default:
			}

			agentConn.SetReadDeadline(time.Time{})
			mt, msg, err := agentConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
					_ = err
				}
				closeShutdown() // Signal other goroutine to exit
				return
			}

			// Send to client with timeout
			clientConn.SetWriteDeadline(time.Time{})
			if err = clientConn.WriteMessage(mt, msg); err != nil {
				closeShutdown()
				return
			}
		}
	}()

	// Wait for both goroutines to complete
	wg.Wait()
}

// CreateSession handles POST /sandboxes/:id/pty/sessions
func (h *PTYHandler) CreateSession(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	// Call agent to create session
	session, err := h.sessionService.CreateSession(c.Request.Context(), sbxInstance)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to create session", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Session created", session))
}

// ListSessions handles GET /sandboxes/:id/pty/sessions
func (h *PTYHandler) ListSessions(c *gin.Context) {
	id := c.Param("id")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	// Call agent to list sessions
	sessions, err := h.sessionService.ListSessions(c.Request.Context(), sbxInstance)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to list sessions", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sessions retrieved", sessions))
}

// ConnectSession handles WebSocket connection to a persistent session
func (h *PTYHandler) ConnectSession(c *gin.Context) {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	// Upgrade client connection to WebSocket
	clientConn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()

	// Connect to agent's session WebSocket
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	agentURL := fmt.Sprintf("ws://%s/pty/sessions/%s", sbxInstance, sessionID)
	agentConn, _, err := h.dialer.DialContext(ctx, agentURL, nil)
	if err != nil {
		return
	}
	defer agentConn.Close()

	// Proxy bidirectionally
	var wg sync.WaitGroup
	shutdownChan := make(chan struct{})
	var shutdownOnce sync.Once
	closeShutdown := func() {
		shutdownOnce.Do(func() {
			close(shutdownChan)
		})
	}

	// Client -> Agent (Input)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-shutdownChan:
				return
			default:
			}

			clientConn.SetReadDeadline(time.Time{})
			mt, msg, err := clientConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
					_ = err
				}
				closeShutdown()
				return
			}

			agentConn.SetWriteDeadline(time.Time{})
			if err = agentConn.WriteMessage(mt, msg); err != nil {
				closeShutdown()
				return
			}
		}
	}()

	// Agent -> Client (Output)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-shutdownChan:
				return
			default:
			}

			agentConn.SetReadDeadline(time.Time{})
			mt, msg, err := agentConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
					_ = err
				}
				closeShutdown()
				return
			}

			clientConn.SetWriteDeadline(time.Time{})
			if err = clientConn.WriteMessage(mt, msg); err != nil {
				closeShutdown()
				return
			}
		}
	}()

	wg.Wait()
}

// DeleteSession handles DELETE /sandboxes/:id/pty/sessions/:sessionId
func (h *PTYHandler) DeleteSession(c *gin.Context) {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	// Call agent to delete session
	if err := h.sessionService.DeleteSession(c.Request.Context(), sbxInstance, sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to delete session", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Session deleted", nil))
}

// ExecuteCommand handles POST /sandboxes/:id/pty/sessions/:sessionId/execute
func (h *PTYHandler) ExecuteCommand(c *gin.Context) {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	// Parse request body
	var req struct {
		Command string `json:"command" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", err.Error()))
		return
	}

	// Call agent to execute command
	if err := h.sessionService.ExecuteCommand(c.Request.Context(), sbxInstance, sessionID, req.Command); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to execute command", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Command sent", nil))
}

// GetBuffer handles GET /sandboxes/:id/pty/sessions/:sessionId/buffer
func (h *PTYHandler) GetBuffer(c *gin.Context) {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	// Call agent to get buffer
	buffer, err := h.sessionService.GetBuffer(c.Request.Context(), sbxInstance, sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to get buffer", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Buffer retrieved", buffer))
}

// ResizeTerminal handles POST /sandboxes/:id/pty/sessions/:sessionId/resize
func (h *PTYHandler) ResizeTerminal(c *gin.Context) {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	sandbox, found := h.sandboxService.Get(c.Request.Context(), id)
	if !found {
		c.JSON(http.StatusNotFound, model.NewErrorResponse("Sandbox not found", ""))
		return
	}

	sbxInstance := sandbox.ID.Hex()

	// Parse request body
	var req struct {
		Rows uint16 `json:"rows" binding:"required"`
		Cols uint16 `json:"cols" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse("Invalid request", err.Error()))
		return
	}

	// Call agent to resize terminal
	if err := h.sessionService.ResizeTerminal(c.Request.Context(), sbxInstance, sessionID, req.Rows, req.Cols); err != nil {
		c.JSON(http.StatusInternalServerError, model.NewErrorResponse("Failed to resize terminal", err.Error()))
		return
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Terminal resized", nil))
}
