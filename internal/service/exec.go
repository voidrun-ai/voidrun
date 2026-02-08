package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/machine"
	"voidrun/pkg/util"
)

// ExecService handles command execution in VMs
type ExecService struct {
	cfg    *config.Config
	client *http.Client
}

// NewExecService creates a new exec service
func NewExecService(cfg *config.Config) *ExecService {
	return &ExecService{
		cfg:    cfg,
		client: GetVMHTTPClient(),
	}
}

// ValidateVMID validates the VM ID format
func (s *ExecService) ValidateVMID(id string) bool {
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return len(id) > 0 && len(id) <= 64
}

// ParseAndValidateRequest parses and validates an exec request
func (s *ExecService) ParseAndValidateRequest(req model.ExecRequest) (cmd string, args []string, timeout int, err error) {
	// Validate command length
	if len(req.Command) > config.MaxCommandLength {
		return "", nil, 0, fmt.Errorf("command too long")
	}
	if len(req.Args) > config.MaxArgsCount {
		return "", nil, 0, fmt.Errorf("too many arguments")
	}

	// Sanitize and bound timeout
	timeout = req.Timeout
	if timeout <= 0 {
		timeout = config.DefaultTimeout
	}
	if timeout > config.MaxTimeout {
		timeout = config.MaxTimeout
	}

	// Parse command if args not provided
	if len(req.Args) == 0 {
		parsedParts, parseErr := util.ParseCommand(req.Command)
		if parseErr != nil {
			return "", nil, 0, fmt.Errorf("command parsing error: %w", parseErr)
		}
		if len(parsedParts) == 0 {
			return "", nil, 0, fmt.Errorf("empty command")
		}
		if len(parsedParts) > config.MaxArgsCount {
			return "", nil, 0, fmt.Errorf("too many arguments after parsing")
		}
		cmd = parsedParts[0]
		args = parsedParts[1:]
	} else {
		cmd = req.Command
		args = req.Args
	}

	if strings.TrimSpace(cmd) == "" {
		return "", nil, 0, fmt.Errorf("empty command name")
	}

	return cmd, args, timeout, nil
}

// ExecuteCommand executes a command in a VM and streams the output
func (s *ExecService) ExecuteCommand(vmID, cmd string, args []string, timeout int, writer io.Writer, flush func()) error {
	// Use common DialVsock helper
	conn, err := machine.DialVsock(vmID, 1024, 2*time.Second)
	if err != nil {
		return fmt.Errorf("VM not reachable: %w", err)
	}
	defer conn.Close()

	// Send request
	conn.SetDeadline(time.Now().Add(time.Duration(timeout) * time.Second))

	agentReq := map[string]interface{}{
		"cmd":     cmd,
		"args":    args,
		"timeout": timeout,
	}
	if err := json.NewEncoder(conn).Encode(agentReq); err != nil {
		return fmt.Errorf("failed to send command: %w", err)
	}

	// Stream response
	buffer := make([]byte, config.ReadBufferSize)
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			writer.Write(buffer[:n])
			if flush != nil {
				flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[exec] VM %s read error: %v", vmID, err)
			}
			break
		}
	}

	return nil
}

// ExecSync executes a command synchronously via agent /exec endpoint and returns the result
func (s *ExecService) ExecSync(ctx context.Context, vmID, command string, timeout int, env map[string]string, cwd string) (*http.Response, error) {
	payload := map[string]interface{}{
		"cmd":     command,
		"timeout": timeout,
	}
	if len(env) > 0 {
		payload["env"] = env
	}
	if strings.TrimSpace(cwd) != "" {
		payload["cwd"] = cwd
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return ExecAgentCommand(ctx, s.client, vmID, bytes.NewReader(body))
}

// ExecStreamSSE executes a command and streams SSE output from the agent /exec-stream endpoint.
func (s *ExecService) ExecStreamSSE(ctx context.Context, vmID, command string, timeout int, env map[string]string, cwd string, writer io.Writer, flush func()) error {
	payload := map[string]interface{}{
		"cmd":     command,
		"timeout": timeout,
	}
	if len(env) > 0 {
		payload["env"] = env
	}
	if strings.TrimSpace(cwd) != "" {
		payload["cwd"] = cwd
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := AgentCommand(ctx, s.client, vmID, bytes.NewReader(body), "/exec-stream", http.MethodPost)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent exec-stream failed: %s", strings.TrimSpace(string(b)))
	}

	buffer := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buffer)
		if n > 0 {
			if _, werr := writer.Write(buffer[:n]); werr != nil {
				return werr
			}
			if flush != nil {
				flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return rerr
		}
	}

	return nil
}

// ExecuteCommandStream executes a command and streams NDJSON output with separate stdout/stderr
func (s *ExecService) ExecuteCommandStream(vmID, cmd string, args []string, timeout int, writer io.Writer, flush func()) error {
	// Use common DialVsock helper
	conn, err := machine.DialVsock(vmID, 1024, 2*time.Second)
	if err != nil {
		return fmt.Errorf("VM not reachable: %w", err)
	}
	defer conn.Close()

	// Send request
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	agentReq := map[string]interface{}{
		"cmd":     cmd,
		"args":    args,
		"timeout": timeout,
	}
	if err := json.NewEncoder(conn).Encode(agentReq); err != nil {
		return fmt.Errorf("failed to send command: %w", err)
	}

	// Clear deadline for streaming
	conn.SetReadDeadline(time.Time{})

	// Proxy NDJSON chunks (agent already sends stdout/stderr separately)
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

// ExecAgentCommand sends a JSON command payload to the agent /exec endpoint over HTTP.
// Shared helper so FS and Exec services reuse identical logic.
func ExecAgentCommand(ctx context.Context, client *http.Client, vmID string, body io.Reader) (*http.Response, error) {
	return AgentCommand(ctx, client, vmID, body, "/exec", http.MethodPost)
}

func AgentCommand(ctx context.Context, client *http.Client, vmID string, body io.Reader, path string, method string) (*http.Response, error) {
	if client == nil {
		client = GetVMHTTPClient()
	}

	u := url.URL{
		Scheme: "http",
		Host:   vmID,
		Path:   path,
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	return client.Do(req)
}
