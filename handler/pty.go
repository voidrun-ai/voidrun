package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"voidrun/model"
	"voidrun/service"
	"voidrun/util"

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

// Proxy handles the ephemeral PTY WebSocket connection.
// WebSocket safety: once Upgrade() is called the response is owned by the WS
// protocol — return nil after that point, errors before are surfaced as JSON.
func (h *PTYHandler) Proxy(c *gin.Context) error {
	sbxInstance := c.Param("id")

	clientConn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrader already wrote an HTTP error response; WriteError will no-op.
		return nil
	}
	defer clientConn.Close()

	// Track active connection for this sandbox
	if connTracker != nil {
		connTracker.Acquire(sbxInstance)
		defer connTracker.Release(sbxInstance)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	agentConn, _, err := h.dialer.DialContext(ctx, "ws://"+sbxInstance+"/pty", nil)
	if err != nil {
		return nil // client is already upgraded; nothing useful to write
	}
	defer agentConn.Close()

	proxyWebSocket(clientConn, agentConn)
	return nil
}

// CreateSession handles POST /sandboxes/:id/pty/sessions
func (h *PTYHandler) CreateSession(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	session, err := h.sessionService.CreateSession(c.Request.Context(), id)
	if err != nil {
		return util.ErrInternal("Failed to create session", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Session created", session))
	return nil
}

// ListSessions handles GET /sandboxes/:id/pty/sessions
func (h *PTYHandler) ListSessions(c *gin.Context) error {
	id := c.Param("id")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	sessions, err := h.sessionService.ListSessions(c.Request.Context(), id)
	if err != nil {
		return util.ErrInternal("Failed to list sessions", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Sessions retrieved", sessions))
	return nil
}

// ConnectSession handles WebSocket connection to a persistent session.
// Same WebSocket safety note as Proxy.
func (h *PTYHandler) ConnectSession(c *gin.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	clientConn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return nil // upgrader wrote error
	}
	defer clientConn.Close()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	agentURL := fmt.Sprintf("ws://%s/pty/sessions/%s", id, sessionID)
	agentConn, _, err := h.dialer.DialContext(ctx, agentURL, nil)
	if err != nil {
		return nil // already upgraded; nothing to write
	}
	defer agentConn.Close()

	proxyWebSocket(clientConn, agentConn)
	return nil
}

// DeleteSession handles DELETE /sandboxes/:id/pty/sessions/:sessionId
func (h *PTYHandler) DeleteSession(c *gin.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	if err := h.sessionService.DeleteSession(c.Request.Context(), id, sessionID); err != nil {
		return util.ErrInternal("Failed to delete session", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Session deleted", nil))
	return nil
}

// ExecuteCommand handles POST /sandboxes/:id/pty/sessions/:sessionId/execute
func (h *PTYHandler) ExecuteCommand(c *gin.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req struct {
		Command string `json:"command" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request", err.Error())
	}

	if err := h.sessionService.ExecuteCommand(c.Request.Context(), id, sessionID, req.Command); err != nil {
		return util.ErrInternal("Failed to execute command", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Command sent", nil))
	return nil
}

// GetBuffer handles GET /sandboxes/:id/pty/sessions/:sessionId/buffer
func (h *PTYHandler) GetBuffer(c *gin.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	buffer, err := h.sessionService.GetBuffer(c.Request.Context(), id, sessionID)
	if err != nil {
		return util.ErrInternal("Failed to get buffer", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Buffer retrieved", buffer))
	return nil
}

// ResizeTerminal handles POST /sandboxes/:id/pty/sessions/:sessionId/resize
func (h *PTYHandler) ResizeTerminal(c *gin.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionId")

	if err := ensureSandboxRunning(c, h.sandboxService, id); err != nil {
		return err
	}

	var req struct {
		Rows uint16 `json:"rows" binding:"required"`
		Cols uint16 `json:"cols" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return util.ErrBadRequest("Invalid request", err.Error())
	}

	if err := h.sessionService.ResizeTerminal(c.Request.Context(), id, sessionID, req.Rows, req.Cols); err != nil {
		return util.ErrInternal("Failed to resize terminal", err)
	}

	c.JSON(http.StatusOK, model.NewSuccessResponse("Terminal resized", nil))
	return nil
}

// ---------------------------------------------------------------------------
// proxyWebSocket — shared bidirectional relay for Proxy and ConnectSession
// ---------------------------------------------------------------------------

// proxyWebSocket relays messages bidirectionally between client and agent
// WebSocket connections until either side closes.
func proxyWebSocket(client, agent *websocket.Conn) {
	shutdownChan := make(chan struct{})
	var shutdownOnce sync.Once
	closeShutdown := func() {
		shutdownOnce.Do(func() { close(shutdownChan) })
	}

	var wg sync.WaitGroup

	relay := func(src, dst *websocket.Conn, label string) {
		defer wg.Done()
		for {
			select {
			case <-shutdownChan:
				return
			default:
			}
			src.SetReadDeadline(time.Time{})
			mt, msg, err := src.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
					log.Printf("[PTY] %s connection closed unexpectedly: %v", label, err)
				}
				closeShutdown()
				return
			}
			dst.SetWriteDeadline(time.Time{})
			if err = dst.WriteMessage(mt, msg); err != nil {
				closeShutdown()
				return
			}
		}
	}

	wg.Add(2)
	go relay(client, agent, "client→agent")
	go relay(agent, client, "agent→client")
	wg.Wait()
}
