package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// PTYSessionService handles PTY session management via agent HTTP endpoints
type PTYSessionService struct {
	client *http.Client
}

// NewPTYSessionService creates a new PTY session service
func NewPTYSessionService() *PTYSessionService {
	return &PTYSessionService{
		client: GetSandboxHTTPClient(),
	}
}

// PTYSessionResponse represents a session creation response
type PTYSessionResponse struct {
	SessionID string `json:"sessionId"`
	CreatedAt string `json:"createdAt"`
}

// PTYSessionInfo represents session information
type PTYSessionInfo struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
	Clients   int    `json:"clients"`
	Alive     bool   `json:"alive"`
}

// PTYSessionsListResponse represents the list sessions response
type PTYSessionsListResponse struct {
	Sessions []PTYSessionInfo `json:"sessions"`
}

// PTYBufferResponse represents the buffer response
type PTYBufferResponse struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
}

// PTYCommandResponse represents the execute command response
type PTYCommandResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// PTYResizeResponse represents the resize response
type PTYResizeResponse struct {
	Success bool `json:"success"`
}

// agentPTYSessionResponse represents a session creation response from the agent
type agentPTYSessionResponse struct {
	Success   bool   `json:"success"`
	SessionID string `json:"session_id"`
	CreatedAt string `json:"created_at"`
}

func (s *PTYSessionService) CreateSession(ctx context.Context, sbxInstance string) (*PTYSessionResponse, error) {
	u := url.URL{
		Scheme: "http",
		Host:   sbxInstance,
		Path:   "/pty/sessions",
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent returned %d: %s", resp.StatusCode, body)
	}

	var agentResult agentPTYSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&agentResult); err != nil {
		return nil, fmt.Errorf("failed to decode agent response: %w", err)
	}

	if !agentResult.Success {
		return nil, fmt.Errorf("agent reported failure creating PTY session")
	}

	// If agent returned empty sessionId, fetch the sessions list to get the most recent one
	if agentResult.SessionID == "" {
		listResp, err := s.ListSessions(ctx, sbxInstance)
		if err != nil {
			return nil, fmt.Errorf("session created but failed to fetch session details: %w", err)
		}
		if len(listResp.Sessions) == 0 {
			return nil, fmt.Errorf("session created but not found in list")
		}
		// Return the most recently created session (last in list)
		lastSession := listResp.Sessions[len(listResp.Sessions)-1]
		return &PTYSessionResponse{
			SessionID: lastSession.ID,
			CreatedAt: lastSession.CreatedAt,
		}, nil
	}

	return &PTYSessionResponse{
		SessionID: agentResult.SessionID,
		CreatedAt: agentResult.CreatedAt,
	}, nil
}

// agentPTYSessionInfo represents session information from the agent
type agentPTYSessionInfo struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Clients   int    `json:"clients"`
	Alive     bool   `json:"alive"`
}

// agentPTYSessionsListResponse represents the list sessions response from the agent
type agentPTYSessionsListResponse struct {
	Success  bool                  `json:"success"`
	Sessions []agentPTYSessionInfo `json:"sessions"`
}

func (s *PTYSessionService) ListSessions(ctx context.Context, sbxInstance string) (*PTYSessionsListResponse, error) {
	u := url.URL{
		Scheme: "http",
		Host:   sbxInstance,
		Path:   "/pty/sessions",
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent returned %d: %s", resp.StatusCode, body)
	}

	var agentResult agentPTYSessionsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&agentResult); err != nil {
		return nil, fmt.Errorf("failed to decode agent response: %w", err)
	}

	if !agentResult.Success {
		return nil, fmt.Errorf("agent reported failure listing PTY sessions")
	}

	sessions := make([]PTYSessionInfo, len(agentResult.Sessions))
	for i, s := range agentResult.Sessions {
		sessions[i] = PTYSessionInfo{
			ID:        s.ID,
			CreatedAt: s.CreatedAt,
			Clients:   s.Clients,
			Alive:     s.Alive,
		}
	}

	return &PTYSessionsListResponse{Sessions: sessions}, nil
}

// DeleteSession deletes a PTY session
func (s *PTYSessionService) DeleteSession(ctx context.Context, sbxInstance, sessionID string) error {
	u := url.URL{
		Scheme: "http",
		Host:   sbxInstance,
		Path:   fmt.Sprintf("/pty/sessions/%s", sessionID),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, body)
	}

	return nil
}

// ExecuteCommand sends a command to a PTY session
func (s *PTYSessionService) ExecuteCommand(ctx context.Context, sbxInstance, sessionID, command string) error {
	u := url.URL{
		Scheme: "http",
		Host:   sbxInstance,
		Path:   fmt.Sprintf("/pty/sessions/%s/execute", sessionID),
	}

	payload := map[string]string{
		"command": command,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, respBody)
	}

	return nil
}

// GetBuffer retrieves the session's output buffer
func (s *PTYSessionService) GetBuffer(ctx context.Context, sbxInstance, sessionID string) (*PTYBufferResponse, error) {
	u := url.URL{
		Scheme: "http",
		Host:   sbxInstance,
		Path:   fmt.Sprintf("/pty/sessions/%s/buffer", sessionID),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent returned %d: %s", resp.StatusCode, body)
	}

	var result PTYBufferResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// ResizeTerminal resizes the terminal dimensions
func (s *PTYSessionService) ResizeTerminal(ctx context.Context, sbxInstance, sessionID string, rows, cols uint16) error {
	u := url.URL{
		Scheme: "http",
		Host:   sbxInstance,
		Path:   fmt.Sprintf("/pty/sessions/%s/resize", sessionID),
	}

	payload := map[string]uint16{
		"rows": rows,
		"cols": cols,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a longer timeout for resize operations
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req = req.WithContext(ctxWithTimeout)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, respBody)
	}

	return nil
}
