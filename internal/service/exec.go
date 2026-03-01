package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/internal/sandboxclient"
	"voidrun/pkg/machine"
	"voidrun/pkg/util"
)

type ExecService struct {
	cfg    *config.Config
	client *http.Client
}

// NewExecService creates a new exec service
func NewExecService(cfg *config.Config) *ExecService {
	return &ExecService{
		cfg:    cfg,
		client: sandboxclient.GetSandboxHTTPClient(),
	}
}

// ValidateSandboxID validates the sandbox ID format
func (s *ExecService) ValidateSandboxID(id string) bool {
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

// ExecuteCommand executes a command in a sandbox and streams the output
func (s *ExecService) ExecuteCommand(sbxID, cmd string, args []string, timeout int, writer io.Writer, flush func()) error {
	// Use common DialVsock helper
	conn, err := machine.DialVsock(sbxID, 1024, 2*time.Second)
	if err != nil {
		return fmt.Errorf("sandbox not reachable: %w", err)
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
				log.Printf("[exec] sandbox %s read error: %v", sbxID, err)
			}
			break
		}
	}

	return nil
}

// ExecSync executes a command synchronously via agent /exec endpoint and returns the result
func (s *ExecService) ExecSync(ctx context.Context, sbxID string, command string, timeout int, env map[string]string, cwd string) (*http.Response, error) {
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

	return ExecAgentCommand(ctx, s.client, sbxID, bytes.NewReader(body))
}

// ExecStreamSSE executes a command and streams SSE output from the agent /exec-stream endpoint.
func (s *ExecService) ExecStreamSSE(ctx context.Context, sbxID string, command string, timeout int, env map[string]string, cwd string, writer io.Writer, flush func()) error {
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

	resp, err := AgentCommand(ctx, s.client, sbxID, bytes.NewReader(body), "/exec-stream", http.MethodPost)
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

// ExecAgentCommand sends a JSON command payload to the agent /exec endpoint over HTTP.
// Shared helper so FS and Exec services reuse identical logic.
func ExecAgentCommand(ctx context.Context, client *http.Client, sbxID string, body io.Reader) (*http.Response, error) {
	return AgentCommand(ctx, client, sbxID, body, "/exec", http.MethodPost)
}

// Constants to replace magic numbers, making the code cleaner.
const (
	agentReadyTimeout = 30 * time.Second
	agentPollInterval = 50 * time.Millisecond
	agentDialTimeout  = 150 * time.Millisecond
	agentPort         = 1024
)

func AgentCommand(ctx context.Context, client *http.Client, sbxID string, body io.Reader, path string, method string) (*http.Response, error) {
	if client == nil {
		client = sandboxclient.GetSandboxHTTPClient()
	}

	// 1. Buffer the body in case we need to retry the request.
	var bodyBytes []byte
	if body != nil {
		var err error
		if bodyBytes, err = io.ReadAll(body); err != nil {
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}
	}

	// 2. Safely construct the URL (handles query params correctly)
	if path != "" && path[0] != '/' {
		path = "/" + path
	}
	reqURL := "http://" + sbxID + path

	// 3. Helper closure to create and execute the HTTP request
	doRequest := func() (*http.Response, error) {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		return client.Do(req)
	}

	// 4. First Attempt
	resp, err := doRequest()
	if err == nil {
		return resp, nil
	}

	// 5. Evaluate if we should retry
	// Keep readiness probe paths untouched to avoid recursive auto-start loops.
	if path == "" || path == "/" || !shouldAutoStartOnAgentError(err) {
		return nil, err
	}

	// 6. Start/Resume Machine and Retry
	if ensureErr := ensureSandboxRunning(ctx, sbxID); ensureErr != nil {
		// Note: Requires Go 1.20+ for multiple %w wrapping
		return nil, fmt.Errorf("agent command failed: %w; auto-start failed: %w", err, ensureErr)
	}

	return doRequest()
}

func shouldAutoStartOnAgentError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func ensureSandboxRunning(ctx context.Context, sbxID string) error {
	// Attempt Start, fallback to Resume
	if err := machine.Start(sbxID); err != nil {
		if resumeErr := machine.Resume(sbxID); resumeErr != nil {
			return fmt.Errorf("start failed: %v; resume failed: %w", err, resumeErr)
		}
	}

	// SIMPLIFIED: WithTimeout automatically respects the parent context's
	// deadline if it is shorter than agentReadyTimeout (30s).
	dialCtx, cancel := context.WithTimeout(ctx, agentReadyTimeout)
	defer cancel()

	if err := waitForAgentDial(dialCtx, sbxID); err != nil {
		return fmt.Errorf("agent not ready after auto-start: %w", err)
	}

	return nil
}

func waitForAgentDial(ctx context.Context, sbxID string) error {
	ticker := time.NewTicker(agentPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		// Try dialing
		conn, err := machine.DialVsock(sbxID, agentPort, agentDialTimeout)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err

		// Wait for next tick OR context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout/canceled: %w (last error: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
			// Loop continues
		}
	}
}
