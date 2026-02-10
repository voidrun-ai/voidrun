package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"voidrun/internal/config"
	"voidrun/internal/model"
)

// CommandsService handles process management operations
type CommandsService struct {
	cfg *config.Config
}

// NewCommandsService creates a new commands service
func NewCommandsService(cfg *config.Config) *CommandsService {
	return &CommandsService{cfg: cfg}
}

// Run starts a background process
func (s *CommandsService) Run(sbxInstance string, req model.CommandRunRequest) (*model.CommandRunResponse, error) {
	// Create payload for agent
	payload := map[string]interface{}{
		"command": req.Command,
		"env":     req.Env,
		"cwd":     req.Cwd,
		"timeout": req.Timeout,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Send to agent via HTTP
	resp, err := AgentCommand(context.Background(), nil, sbxInstance, bytes.NewReader(body), "/run", http.MethodPost)
	if err != nil {
		return nil, fmt.Errorf("failed to communicate with agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyText, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent error: %s", string(bodyText))
	}

	var agentResp model.CommandRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("failed to decode agent response: %w", err)
	}

	return &agentResp, nil
}

// List returns all running processes
func (s *CommandsService) List(sbxInstance string) (*model.CommandListResponse, error) {
	// Send request to agent
	resp, err := AgentCommand(context.Background(), nil, sbxInstance, nil, "/processes", http.MethodGet)
	if err != nil {
		return nil, fmt.Errorf("failed to communicate with agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyText, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent error: %s", string(bodyText))
	}

	var agentResp model.CommandListResponse
	if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("failed to decode agent response: %w", err)
	}

	return &agentResp, nil
}

// Kill terminates a process
func (s *CommandsService) Kill(sbxInstance string, pid int) (*model.CommandKillResponse, error) {
	payload := map[string]interface{}{
		"pid": pid,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := AgentCommand(context.Background(), nil, sbxInstance, bytes.NewReader(body), "/kill", http.MethodPost)
	if err != nil {
		return nil, fmt.Errorf("failed to communicate with agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyText, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent error: %s", string(bodyText))
	}

	var agentResp model.CommandKillResponse
	if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("failed to decode agent response: %w", err)
	}

	return &agentResp, nil
}

// Attach streams output from a running process
func (s *CommandsService) Attach(sbxInstance string, pid int, writer io.Writer, flush func()) error {
	payload := map[string]interface{}{
		"pid": pid,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := AgentCommand(context.Background(), nil, sbxInstance, bytes.NewReader(body), "/attach", http.MethodPost)
	if err != nil {
		return fmt.Errorf("failed to communicate with agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyText, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent error: %s", string(bodyText))
	}

	// Proxy response body to writer
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := writer.Write(buf[:n]); werr != nil {
				return werr
			}
			flush()
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	return nil
}

// Wait waits for a process to complete
func (s *CommandsService) Wait(sbxInstance string, pid int) (*model.CommandWaitResponse, error) {
	payload := map[string]interface{}{
		"pid": pid,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := AgentCommand(context.Background(), nil, sbxInstance, bytes.NewReader(body), "/wait", http.MethodPost)
	if err != nil {
		return nil, fmt.Errorf("failed to communicate with agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyText, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent error: %s", string(bodyText))
	}

	var agentResp model.CommandWaitResponse
	if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("failed to decode agent response: %w", err)
	}

	return &agentResp, nil
}
