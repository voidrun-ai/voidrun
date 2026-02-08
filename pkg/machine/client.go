package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Default paths - can be overridden via configuration
const (
	GuestAgentPort = 1024
)

// InstancesRoot is the base path for VM instances (set from config)
var InstancesRoot string

// SetInstancesRoot sets the instances root directory from configuration
func SetInstancesRoot(path string) {
	if path != "" {
		InstancesRoot = path
	}
}

// KernelPath is the path to the kernel image
// var KernelPath = DefaultKernelPath

// APIClient handles communication with Cloud Hypervisor API
type APIClient struct {
	socketPath string
	timeout    time.Duration
}

// NewAPIClient creates a new API client for a VM
func NewAPIClient(socketPath string) *APIClient {
	return &APIClient{
		socketPath: socketPath,
		timeout:    5 * time.Second,
	}
}

// NewAPIClientForVM creates an API client for a VM by ID
func NewAPIClientForVM(vmID string) *APIClient {
	return NewAPIClient(GetSocketPath(vmID))
}

// httpClient creates an HTTP client that connects via Unix socket
func (c *APIClient) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", c.socketPath)
			},
		},
		Timeout: c.timeout,
	}
}

// SendJSON sends a JSON payload to the API
func (c *APIClient) SendJSON(endpoint string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	return c.request(endpoint, data)
}

// Send sends an empty request to the API
func (c *APIClient) Send(endpoint string) error {
	return c.request(endpoint, nil)
}

// Get performs a GET request and returns the response body
func (c *APIClient) Get(endpoint string) ([]byte, error) {
	client := c.httpClient()
	url := fmt.Sprintf("http://localhost/api/v1/%s", endpoint)

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GetState returns the current VM state
func (c *APIClient) GetState() (string, error) {
	body, err := c.Get("vm.info")
	if err != nil {
		return "", err
	}

	var info struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", err
	}
	return info.State, nil
}

// request performs an API request
func (c *APIClient) request(endpoint string, body []byte) error {
	client := c.httpClient()
	url := fmt.Sprintf("http://localhost/api/v1/%s", endpoint)

	var req *http.Request
	var err error

	if body != nil {
		req, err = http.NewRequest(http.MethodPut, url, bytes.NewBuffer(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(http.MethodPut, url, nil)
		if err != nil {
			return err
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		// Ignore "already running/paused" errors
		if strings.Contains(string(respBody), "InvalidStateTransition") {
			return nil
		}
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// IsSocketAvailable checks if the VM socket exists
func (c *APIClient) IsSocketAvailable() bool {
	_, err := os.Stat(c.socketPath)
	return err == nil
}

// WaitForSocket waits for the socket to become available
func (c *APIClient) WaitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.IsSocketAvailable() {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fmt.Errorf("socket timeout after %v", timeout)
}

func (c *APIClient) GetStateWithContext(ctx context.Context) (string, error) {
	// 1. Setup Transport
	// We define a custom transport here to ensure 'DisableKeepAlives' is true
	// and to ensure the Dial function actually respects the 'ctx' deadline.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			// This is critical: pass 'ctx' to DialContext.
			// If your RefreshStatuses loop times out, this kills the connection attempt instantly.
			return d.DialContext(ctx, "unix", c.socketPath)
		},
		DisableKeepAlives: true, // Essential for 1000 VMs to prevent "too many open files"
	}

	client := &http.Client{
		Transport: transport,
		// We don't set a fixed Timeout here; we rely on the ctx passed by the caller
	}

	// 2. Prepare Request
	url := "http://localhost/api/v1/vm.info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// 3. Execute
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 4. Handle API Errors
	if resp.StatusCode != http.StatusOK {
		// Read a small amount of the body for debugging purposes if it fails
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// 5. Parse Response
	var info struct {
		State string `json:"state"`
	}

	// FIX: Use NewDecoder instead of Unmarshal.
	// resp.Body is an io.ReadCloser, NewDecoder reads from it directly.
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("failed to decode state: %w", err)
	}

	return info.State, nil
}

// Path helpers

// GetInstanceDir returns the instance directory for a VM
func GetInstanceDir(vmID string) string {
	return fmt.Sprintf("%s/%s", InstancesRoot, vmID)
}

// GetSocketPath returns the socket path for a VM
func GetSocketPath(vmID string) string {
	return fmt.Sprintf("%s/%s/vm.sock", InstancesRoot, vmID)
}

// GetVsockPath returns the vsock path for a VM
func GetVsockPath(vmID string) string {
	return fmt.Sprintf("%s/%s/vsock.sock", InstancesRoot, vmID)
}

// GetPIDPath returns the PID file path for a VM
func GetPIDPath(vmID string) string {
	return fmt.Sprintf("%s/%s/vm.pid", InstancesRoot, vmID)
}

// GetTapPath returns the TAP file path for a VM
func GetTapPath(vmID string) string {
	return fmt.Sprintf("%s/%s/vm.tap", InstancesRoot, vmID)
}

// UNUSED: GetLogPath returns the log file path for a VM
// func GetLogPath(vmID string) string {
// 	return fmt.Sprintf("%s/%s/vm.log", InstancesRoot, vmID)
// }

// UNUSED: GetOverlayPath returns the overlay disk path for a VM
// func GetOverlayPath(vmID string) string {
// 	return fmt.Sprintf("%s/%s/overlay.qcow2", InstancesRoot, vmID)
// }

// UNUSED: GetSnapshotsDir returns the snapshots directory for a VM
// func GetSnapshotsDir(vmID string) string {
// 	return fmt.Sprintf("%s/%s/snapshots", InstancesRoot, vmID)
// }
