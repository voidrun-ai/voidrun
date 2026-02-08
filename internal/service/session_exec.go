package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/machine"
)

// SessionExecService proxies PTY session actions to the agent without touching legacy exec flow
type SessionExecService struct {
	cfg *config.Config
}

// NewSessionExecService creates a new session exec service
func NewSessionExecService(cfg *config.Config) *SessionExecService {
	return &SessionExecService{cfg: cfg}
}

// GenerateSessionID produces a random, agent-friendly session identifier
func (s *SessionExecService) GenerateSessionID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("failed to generate session id: %w", err)
	}
	return "sess-" + hex.EncodeToString(buf[:]), nil
}

// ValidateSessionRequest enforces required fields by action and normalizes defaults
func (s *SessionExecService) ValidateSessionRequest(req *model.SessionExecRequest) error {
	allowed := map[string]bool{
		"create": true,
		"exec":   true,
		"input":  true,
		"resize": true,
		"close":  true,
	}
	if !allowed[req.Action] {
		return fmt.Errorf("invalid action")
	}

	if req.Action != "create" && strings.TrimSpace(req.SessionID) == "" {
		return fmt.Errorf("sessionId is required for %s", req.Action)
	}

	if req.Action == "exec" && strings.TrimSpace(req.Command) == "" {
		return fmt.Errorf("command is required for exec")
	}

	if req.Action == "resize" {
		if req.Cols == 0 || req.Rows == 0 {
			return fmt.Errorf("cols and rows are required for resize")
		}
	}

	if req.Action == "create" && strings.TrimSpace(req.SessionID) == "" {
		generated, err := s.GenerateSessionID()
		if err != nil {
			return err
		}
		req.SessionID = generated
	}

	return nil
}

// Send forwards the session action to the VM agent and returns the agent's response
func (s *SessionExecService) Send(vmID string, req model.SessionExecRequest) (*model.SessionExecResponse, error) {
	if err := s.ValidateSessionRequest(&req); err != nil {
		return nil, err
	}

	// Use common DialVsock helper
	conn, err := machine.DialVsock(vmID, 1024, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("VM not reachable: %w", err)
	}
	defer conn.Close()

	// Send session request
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send session request: %w", err)
	}

	var agentResp model.SessionExecResponse
	if err := json.NewDecoder(conn).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("failed to read session response: %w", err)
	}

	if !agentResp.Success {
		if agentResp.Error != "" {
			s.logSessionEvent(vmID, req.SessionID, req.Action, req.Command, req.Input, agentResp.Output, agentResp.Error)
			return &agentResp, fmt.Errorf("%s", agentResp.Error)
		}
		s.logSessionEvent(vmID, req.SessionID, req.Action, req.Command, req.Input, agentResp.Output, "session action failed")
		return &agentResp, fmt.Errorf("session action failed")
	}

	s.logSessionEvent(vmID, req.SessionID, req.Action, req.Command, req.Input, agentResp.Output, "")

	return &agentResp, nil
}

// StreamExec sends an exec_stream action and proxies NDJSON chunks to the client
func (s *SessionExecService) StreamExec(vmID, sessionID, command string, writer io.Writer, flush func()) error {
	// Use common DialVsock helper
	conn, err := machine.DialVsock(vmID, 1024, 2*time.Second)
	if err != nil {
		return fmt.Errorf("VM not reachable: %w", err)
	}
	defer conn.Close()

	// Send exec_stream request
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := map[string]interface{}{
		"action":    "exec_stream",
		"sessionId": sessionID,
		"command":   command,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("failed to send stream request: %w", err)
	}

	// Proxy NDJSON chunks
	// Clear read deadline to allow long-lived streaming without timeouts
	conn.SetReadDeadline(time.Time{})
	buffer := make([]byte, 4096)
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			if _, werr := writer.Write(buffer[:n]); werr == nil && flush != nil {
				flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
	}
	return nil
}

// logSessionEvent appends a structured line per session action under the instance directory
func (s *SessionExecService) logSessionEvent(vmID, sessionID, action, command, input, output, errMsg string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	base := filepath.Join(s.cfg.Paths.InstancesDir, vmID, "session-logs")
	if mkErr := os.MkdirAll(base, 0o755); mkErr != nil {
		return
	}
	path := filepath.Join(base, fmt.Sprintf("%s.log", sessionID))
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if openErr != nil {
		return
	}
	defer f.Close()

	// Limit noisy fields
	trim := func(s string, max int) string {
		if len(s) > max {
			return s[:max] + "..."
		}
		return s
	}

	line := fmt.Sprintf("%s action=%s", time.Now().Format(time.RFC3339), action)
	if command != "" {
		line += fmt.Sprintf(" cmd=%q", trim(command, 512))
	}
	if input != "" {
		line += fmt.Sprintf(" input=%q", trim(input, 512))
	}
	if output != "" {
		line += fmt.Sprintf(" output=%q", trim(output, 1024))
	}
	if errMsg != "" {
		line += fmt.Sprintf(" error=%q", trim(errMsg, 512))
	}
	line += "\n"

	_, _ = f.WriteString(line)
}
