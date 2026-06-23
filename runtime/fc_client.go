package runtime

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

// FCClient is a REST client for the Firecracker microVM management API.
//
// Firecracker exposes its API via a Unix-domain socket.  All configuration
// must be applied before the VM is started (PUT /boot-source, PUT /drives/…,
// etc.).  Once started with PUT /actions {"action_type":"InstanceStart"} the
// only state-change calls available are pause/resume via PATCH /vm and
// shutdown via PUT /actions {"action_type":"SendCtrlAltDel"}.
type FCClient struct {
	socketPath string
	timeout    time.Duration
	httpClient *http.Client
}

// NewFCClient creates a client targeting the Firecracker socket at socketPath.
func NewFCClient(socketPath string) *FCClient {
	c := &FCClient{
		socketPath: socketPath,
		timeout:    10 * time.Second,
	}
	c.httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
			DisableKeepAlives: false,
		},
		Timeout: c.timeout,
	}
	return c
}

// NewFCClientForSandbox creates a client using the canonical socket path for
// a given sandbox ID.
func NewFCClientForSandbox(sandboxID string) *FCClient {
	return NewFCClient(GetSocketPath(sandboxID))
}

// IsSocketAvailable reports whether the Firecracker API socket file exists.
func (c *FCClient) IsSocketAvailable() bool {
	_, err := os.Stat(c.socketPath)
	return err == nil
}

// WaitForSocket polls until the socket appears or the timeout is reached.
func (c *FCClient) WaitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.IsSocketAvailable() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("firecracker socket %s did not appear within %v", c.socketPath, timeout)
}

// ---------------------------------------------------------------------------
// Configuration endpoints (must be called before InstanceStart)
// ---------------------------------------------------------------------------

// PutLogger configures Firecracker logging (PUT /logger).
func (c *FCClient) PutLogger(ctx context.Context, l *FCLogger) error {
	return c.put(ctx, "/logger", l)
}

// PutMetrics configures the Firecracker metrics FIFO (PUT /metrics).
func (c *FCClient) PutMetrics(ctx context.Context, m *FCMetrics) error {
	return c.put(ctx, "/metrics", m)
}

// PutMachineConfig sets vCPUs and memory (PUT /machine-config).
func (c *FCClient) PutMachineConfig(ctx context.Context, mc *FCMachineConfig) error {
	return c.put(ctx, "/machine-config", mc)
}

// PutBootSource sets the kernel image and boot arguments (PUT /boot-source).
func (c *FCClient) PutBootSource(ctx context.Context, bs *FCBootSource) error {
	return c.put(ctx, "/boot-source", bs)
}

// PutDrive attaches or updates a block device (PUT /drives/{drive_id}).
func (c *FCClient) PutDrive(ctx context.Context, d *FCDrive) error {
	return c.put(ctx, "/drives/"+d.DriveID, d)
}

// PutNetworkInterface attaches a TAP-backed NIC (PUT /network-interfaces/{iface_id}).
func (c *FCClient) PutNetworkInterface(ctx context.Context, iface *FCNetworkInterface) error {
	return c.put(ctx, "/network-interfaces/"+iface.IfaceID, iface)
}

// PutVsock configures the virtio-vsock device (PUT /vsock).
func (c *FCClient) PutVsock(ctx context.Context, v *FCVsock) error {
	return c.put(ctx, "/vsock", v)
}

// ---------------------------------------------------------------------------
// Lifecycle endpoints
// ---------------------------------------------------------------------------

// InstanceStart boots the VM (PUT /actions {"action_type":"InstanceStart"}).
// This is idempotent once the VM is running (Firecracker returns 400 in that
// case; the client silently ignores the error).
func (c *FCClient) InstanceStart(ctx context.Context) error {
	return c.put(ctx, "/actions", &FCInstanceAction{ActionType: FCActionInstanceStart})
}

// SendCtrlAltDel requests a graceful shutdown (PUT /actions {"action_type":"SendCtrlAltDel"}).
func (c *FCClient) SendCtrlAltDel(ctx context.Context) error {
	return c.put(ctx, "/actions", &FCInstanceAction{ActionType: FCActionSendCtrlAltDel})
}

// PauseVM pauses a running VM (PATCH /vm {"state":"Paused"}).
func (c *FCClient) PauseVM(ctx context.Context) error {
	return c.patch(ctx, "/vm", &FCVMState{State: "Paused"})
}

// ResumeVM resumes a paused VM (PATCH /vm {"state":"Resumed"}).
func (c *FCClient) ResumeVM(ctx context.Context) error {
	return c.patch(ctx, "/vm", &FCVMState{State: "Resumed"})
}

// ---------------------------------------------------------------------------
// Query endpoints
// ---------------------------------------------------------------------------

// DescribeInstance fetches general instance information (GET /).
func (c *FCClient) DescribeInstance(ctx context.Context) (*FCInstanceInfo, error) {
	body, err := c.get(ctx, "/")
	if err != nil {
		return nil, err
	}
	var info FCInstanceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("fc describe-instance: unmarshal: %w", err)
	}
	return &info, nil
}

// GetState returns the raw state string ("Not started", "Running", "Paused").
func (c *FCClient) GetState(ctx context.Context) (string, error) {
	info, err := c.DescribeInstance(ctx)
	if err != nil {
		return "", err
	}
	return info.State, nil
}

// ---------------------------------------------------------------------------
// Internal HTTP helpers
// ---------------------------------------------------------------------------

func (c *FCClient) put(ctx context.Context, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("fc PUT %s marshal: %w", path, err)
	}
	return c.doRequest(ctx, http.MethodPut, path, data)
}

func (c *FCClient) patch(ctx context.Context, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("fc PATCH %s marshal: %w", path, err)
	}
	return c.doRequest(ctx, http.MethodPatch, path, data)
}

func (c *FCClient) get(ctx context.Context, path string) ([]byte, error) {
	url := "http://localhost" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fc GET %s: new request: %w", path, err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fc GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fc GET %s: status %d: %s", path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (c *FCClient) doRequest(ctx context.Context, method, path string, body []byte) error {
	url := "http://localhost" + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("fc %s %s: new request: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fc %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(respBody))

	// Firecracker returns 400 for already-running state transitions; treat as no-op.
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(msg, "already") {
		return nil
	}

	return fmt.Errorf("fc %s %s: status %d: %s", method, path, resp.StatusCode, msg)
}
