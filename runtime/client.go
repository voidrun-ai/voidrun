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

func NewAPIClient(socketPath string) *APIClient {
	return &APIClient{
		socketPath: socketPath,
		timeout:    5 * time.Second,
	}
}

func NewAPIClientForSandbox(sandboxID string) *APIClient {
	return NewAPIClient(GetSocketPath(sandboxID))
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
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.socketPath)
		},
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// 5. Parse Response
	var info struct {
		State string `json:"state"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("failed to decode state: %w", err)
	}

	return info.State, nil
}

func GetInstanceDir(sbxID string) string {
	return fmt.Sprintf("%s/%s", InstancesRoot, sbxID)
}

func GetSocketPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/vm.sock", InstancesRoot, sbxID)
}

func GetVsockPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/vsock.sock", InstancesRoot, sbxID)
}

func GetPIDPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/vm.pid", InstancesRoot, sbxID)
}

func GetTapPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/vm.tap", InstancesRoot, sbxID)
}

func GetLogPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/vm.log", InstancesRoot, sbxID)
}

func GetOverlayPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/overlay.qcow2", InstancesRoot, sbxID)
}

func GetSnapshotsRoot() string {
	return fmt.Sprintf("%s/snapshots", InstancesRoot)
}

func GetSnapshotsDir(sbxID string) string {
	return fmt.Sprintf("%s/%s", GetSnapshotsRoot(), sbxID)
}

func GetSnapshotTempDir(sbxID string) string {
	return fmt.Sprintf("%s/%s/.tmp", GetSnapshotsRoot(), sbxID)
}
