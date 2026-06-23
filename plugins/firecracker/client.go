package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

type Client struct {
	socketPath string
	timeout    time.Duration
}

func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath, timeout: 10 * time.Second}
}

func (c *Client) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.socketPath)
			},
		},
		Timeout: c.timeout,
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, r)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

func (c *Client) put(ctx context.Context, path string, body any) error {
	_, status, err := c.do(ctx, http.MethodPut, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("firecracker API %s: status %d", path, status)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	data, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("firecracker API %s: status %d", path, status)
	}
	return data, nil
}

func (c *Client) IsSocketAvailable() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c *Client) WaitForSocket(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.IsSocketAvailable() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("firecracker socket timeout")
}

type vmStateResp struct {
	State string `json:"state"`
}

func (c *Client) GetState(ctx context.Context) (string, error) {
	data, err := c.get(ctx, "/")
	if err != nil {
		return "", err
	}
	var s vmStateResp
	if err := json.Unmarshal(data, &s); err != nil {
		return "", err
	}
	return s.State, nil
}

func (c *Client) ConfigureBoot(ctx context.Context, kernel, cmdline, initrd string) error {
	body := map[string]string{
		"kernel_image_path": kernel,
		"boot_args":         cmdline,
	}
	if initrd != "" {
		body["initrd_path"] = initrd
	}
	return c.put(ctx, "/boot-source", body)
}

func (c *Client) ConfigureMachine(ctx context.Context, vcpu, memMB int) error {
	return c.put(ctx, "/machine-config", map[string]any{
		"vcpu_count":   vcpu,
		"mem_size_mib": memMB,
	})
}

func (c *Client) ConfigureDrive(ctx context.Context, path string) error {
	return c.put(ctx, "/drives/root", map[string]any{
		"drive_id":       "root",
		"path_on_host":   path,
		"is_root_device": true,
		"is_read_only":   false,
	})
}

func (c *Client) ConfigureNet(ctx context.Context, tap, mac string) error {
	return c.put(ctx, "/network-interfaces/eth0", map[string]any{
		"iface_id":      "eth0",
		"host_dev_name": tap,
		"guest_mac":     mac,
	})
}

func (c *Client) ConfigureVsock(ctx context.Context, cid uint32, udsPath string) error {
	return c.put(ctx, "/vsock", map[string]any{
		"guest_cid": cid,
		"uds_path":  udsPath,
	})
}

func (c *Client) InstanceStart(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "InstanceStart"})
}

func (c *Client) SendCtrlAltDel(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "SendCtrlAltDel"})
}

func (c *Client) Pause(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "Pause"})
}

func (c *Client) Resume(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "Resume"})
}

func (c *Client) CreateSnapshot(ctx context.Context, snapPath string) error {
	return c.put(ctx, "/snapshot/create", map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": snapPath,
	})
}

func (c *Client) LoadSnapshot(ctx context.Context, snapPath string) error {
	return c.put(ctx, "/snapshot/load", map[string]any{
		"snapshot_path":  snapPath,
		"mem_backend":    map[string]string{"backend_type": "File", "backend_path": filepath.Join(snapPath, "mem_file")},
		"enable_diff_snapshots": false,
	})
}
