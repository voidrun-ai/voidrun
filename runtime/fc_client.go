package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// FCClient is an HTTP client for the Firecracker REST API over a Unix socket.
//
// Firecracker's API base URL is http://localhost/ (no /api/v1/ prefix).
// All configuration endpoints use PUT; the pause/resume endpoint uses PATCH.
type FCClient struct {
	socketPath string
	httpClient *http.Client
}

// NewFCClient creates a Firecracker client for the given Unix socket path.
func NewFCClient(socketPath string) *FCClient {
	c := &FCClient{socketPath: socketPath}
	c.httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		},
		Timeout: 10 * time.Second,
	}
	return c
}

// NewFCClientForSandbox creates a Firecracker client for a specific sandbox.
func NewFCClientForSandbox(sandboxID string) *FCClient {
	return NewFCClient(GetFCSocketPath(sandboxID))
}

// ============================================================================
// Low-level helpers
// ============================================================================

func (c *FCClient) do(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	url := "http://localhost" + path

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("FC API error %d: %s", resp.StatusCode, respBody)
	}

	return respBody, nil
}

func (c *FCClient) put(ctx context.Context, path string, body interface{}) error {
	_, err := c.do(ctx, http.MethodPut, path, body)
	return err
}

func (c *FCClient) patch(ctx context.Context, path string, body interface{}) error {
	_, err := c.do(ctx, http.MethodPatch, path, body)
	return err
}

func (c *FCClient) get(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// ============================================================================
// Configuration endpoints
// ============================================================================

// PutLogger configures the Firecracker logger.
func (c *FCClient) PutLogger(ctx context.Context, cfg *FCLogger) error {
	return c.put(ctx, "/logger", cfg)
}

// PutMachineConfig sets the vCPU count and memory size.
func (c *FCClient) PutMachineConfig(ctx context.Context, cfg *FCMachineConfig) error {
	return c.put(ctx, "/machine-config", cfg)
}

// PutBootSource sets the kernel image path and boot arguments.
func (c *FCClient) PutBootSource(ctx context.Context, src *FCBootSource) error {
	return c.put(ctx, "/boot-source", src)
}

// PutDrive configures a block device.
func (c *FCClient) PutDrive(ctx context.Context, drive *FCDrive) error {
	return c.put(ctx, "/drives/"+drive.DriveID, drive)
}

// PutNetworkInterface configures a network interface.
func (c *FCClient) PutNetworkInterface(ctx context.Context, iface *FCNetworkInterface) error {
	return c.put(ctx, "/network-interfaces/"+iface.IfaceID, iface)
}

// PutVsock configures the vsock device.
func (c *FCClient) PutVsock(ctx context.Context, vsock *FCVsock) error {
	return c.put(ctx, "/vsock", vsock)
}

// ============================================================================
// Lifecycle endpoints
// ============================================================================

// InstanceStart boots the pre-configured microVM.
func (c *FCClient) InstanceStart(ctx context.Context) error {
	return c.put(ctx, "/actions", &FCAction{ActionType: FCActionInstanceStart})
}

// SendCtrlAltDel triggers an ACPI power-off event in the guest.
func (c *FCClient) SendCtrlAltDel(ctx context.Context) error {
	return c.put(ctx, "/actions", &FCAction{ActionType: FCActionSendCtrlAltDel})
}

// PauseVM suspends the microVM.
func (c *FCClient) PauseVM(ctx context.Context) error {
	return c.patch(ctx, "/vm", &FCVMState{State: FCVMStatePaused})
}

// ResumeVM resumes a paused microVM.
func (c *FCClient) ResumeVM(ctx context.Context) error {
	return c.patch(ctx, "/vm", &FCVMState{State: FCVMStateResumed})
}

// ============================================================================
// Information endpoints
// ============================================================================

// GetInstanceInfo returns the current microVM state and metadata.
func (c *FCClient) GetInstanceInfo(ctx context.Context) (*FCInstanceInfo, error) {
	body, err := c.get(ctx, "/")
	if err != nil {
		return nil, err
	}

	var info FCInstanceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse instance info: %w", err)
	}
	return &info, nil
}

// ============================================================================
// Availability helpers
// ============================================================================

// IsSocketAvailable checks whether the Firecracker Unix socket is reachable.
func (c *FCClient) IsSocketAvailable() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, 150*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitForSocket blocks until the socket is available or the deadline is reached.
func (c *FCClient) WaitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.IsSocketAvailable() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("FC socket not available after %v", timeout)
}
