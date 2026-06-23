package runtime

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

// DialVsock dials the guest agent on the given port.
// The vsock protocol differs between hypervisors:
//
//   - Cloud Hypervisor: single UDS at vsock.sock; send "CONNECT <port>\n" and
//     receive "OK\n" before data can flow (proxy/mux protocol).
//   - Firecracker: separate UDS file per port at vsock.sock_<port>; connect
//     directly with no handshake.
//
// The hypervisor is determined by reading the vm.hypervisor marker file so no
// call-site changes are required.
func DialVsock(sbxID string, port uint32, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	hvType := ReadHypervisorType(sbxID)
	if HypervisorType(hvType) == HypervisorFirecracker {
		return dialVsockFC(sbxID, port, timeout)
	}
	return dialVsockCLH(sbxID, port, timeout)
}

// Probe is a lightweight readiness check: dial + optional handshake + close.
// Returns nil if the guest agent on the given port is reachable.
func Probe(sbxID string, port uint32, timeout time.Duration) error {
	hvType := ReadHypervisorType(sbxID)
	if HypervisorType(hvType) == HypervisorFirecracker {
		return probeFC(sbxID, port, timeout)
	}
	return probeCLH(sbxID, port, timeout)
}

// ExecuteCommand sends a JSON command to the guest agent and returns its response.
func ExecuteCommand(sbxID string, cmd string, args []string) (*AgentResponse, error) {
	conn, err := DialVsock(sbxID, GuestAgentPort, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := AgentRequest{Cmd: cmd, Args: args}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send command: %w", err)
	}

	var agentResp AgentResponse
	if err := json.NewDecoder(conn).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &agentResp, nil
}

// ---------------------------------------------------------------------------
// Cloud Hypervisor vsock (proxy/mux protocol)
// ---------------------------------------------------------------------------

// dialVsockCLH connects to the CLH vsock multiplexer socket and performs the
// CONNECT <port>\n handshake.
func dialVsockCLH(sbxID string, port uint32, timeout time.Duration) (net.Conn, error) {
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

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline failed: %w", err)
	}

	handshake := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := io.WriteString(conn, handshake); err != nil {
		return nil, fmt.Errorf("handshake write failed: %w", err)
	}

	// Read response byte-by-byte to avoid consuming any subsequent data.
	var line strings.Builder
	line.Grow(32)
	buf := [1]byte{}
	for {
		if _, err := conn.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("handshake read failed: %w", err)
		}
		if buf[0] == '\n' {
			break
		}
		line.WriteByte(buf[0])
		if line.Len() > 64 {
			return nil, fmt.Errorf("handshake response exceeded 64 bytes")
		}
	}

	response := strings.TrimSpace(line.String())
	if !strings.HasPrefix(response, "OK") {
		return nil, fmt.Errorf("vsock handshake failed, server replied: %q", response)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("failed to clear deadline: %w", err)
	}

	result := conn
	conn = nil
	return result, nil
}

// probeCLH checks reachability via the CLH vsock handshake.
func probeCLH(sbxID string, port uint32, timeout time.Duration) error {
	socketPath := GetVsockPath(sbxID)

	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return err
	}

	var buf [16]byte
	n, err := conn.Read(buf[:])
	if err != nil {
		return err
	}
	if n < 2 || buf[0] != 'O' || buf[1] != 'K' {
		return fmt.Errorf("agent not ready: %q", string(buf[:n]))
	}

	return nil
}

// ---------------------------------------------------------------------------
// Firecracker vsock (per-port UDS files, no handshake)
// ---------------------------------------------------------------------------

// dialVsockFC connects to the per-port UDS that Firecracker creates for
// outbound guest connections: <vsock.sock>_<port>.
func dialVsockFC(sbxID string, port uint32, timeout time.Duration) (net.Conn, error) {
	socketPath := fcVsockPortPath(sbxID, port)
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("fc vsock socket not found at %s: %w", socketPath, err)
	}

	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("fc vsock dial failed: %w", err)
	}
	return conn, nil
}

// probeFC checks reachability of the per-port Firecracker UDS.
func probeFC(sbxID string, port uint32, timeout time.Duration) error {
	socketPath := fcVsockPortPath(sbxID, port)

	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// fcVsockPortPath returns the host-side UDS path for a Firecracker vsock port.
// Firecracker creates this file when the guest connects from that port number.
func fcVsockPortPath(sbxID string, port uint32) string {
	return fmt.Sprintf("%s_%d", GetVsockPath(sbxID), port)
}
