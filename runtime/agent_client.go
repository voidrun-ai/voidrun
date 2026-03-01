package machine

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// AgentResponse represents a response from the guest agent
type AgentResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

// AgentRequest represents a command request to the guest agent
type AgentRequest struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

func DialVsock(sbxID string, port uint32, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	socketPath := GetVsockPath(sbxID)
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("vsock socket not found: %w", err)
	}

	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to dial vsock unix socket: %w", err)
	}

	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	// Handshake with deadline
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline failed: %w", err)
	}

	// Send handshake
	handshake := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := io.WriteString(conn, handshake); err != nil {
		return nil, fmt.Errorf("handshake write failed: %w", err)
	}

	// Read response byte-by-byte (critical: prevents data loss)
	var line strings.Builder
	line.Grow(32)    // Pre-allocate typical response size
	buf := [1]byte{} // Array avoids heap allocation

	for {
		if _, err := conn.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("handshake read failed: %w", err)
		}

		if buf[0] == '\n' {
			break
		}

		line.WriteByte(buf[0])

		// Safety: Prevent infinite loop on malformed response
		if line.Len() > 64 {
			return nil, fmt.Errorf("handshake response exceeded 64 bytes")
		}
	}

	// Validate response (accept "OK" or "OK <port>")
	response := strings.TrimSpace(line.String())
	if !strings.HasPrefix(response, "OK") {
		return nil, fmt.Errorf("vsock handshake failed, server replied: %q", response)
	}

	// Clear deadline for normal operation
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("failed to clear deadline: %w", err)
	}

	// Success: prevent defer from closing
	result := conn
	conn = nil
	return result, nil
}

func ExecuteCommand(sbxID string, cmd string, args []string) (*AgentResponse, error) {
	// Use the common DialVsock helper
	conn, err := DialVsock(sbxID, GuestAgentPort, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Send JSON Command to Agent
	req := AgentRequest{Cmd: cmd, Args: args}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send command: %w", err)
	}

	// Read Response
	var agentResp AgentResponse
	if err := json.NewDecoder(conn).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &agentResp, nil
}
