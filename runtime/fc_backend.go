package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"voidrun/config"
	"voidrun/model"
	"voidrun/util"
)

// FCBackend implements Hypervisor for Firecracker.
//
// Firecracker exposes a REST API over a Unix socket. Spawning the binary
// boots an empty VMM; the caller must then PUT each resource (boot-source,
// machine-config, drives, network, vsock, logger, metrics) and finally
// trigger an InstanceStart action.
type FCBackend struct {
	cfg *config.Config
}

// NewFCBackend constructs the Firecracker backend.
func NewFCBackend(cfg *config.Config) *FCBackend {
	return &FCBackend{cfg: cfg}
}

func (b *FCBackend) Name() string { return string(HypervisorFirecracker) }

func (b *FCBackend) Capabilities() Capabilities {
	return Capabilities{
		// FC has no live device hotplug.
		SupportsHotplugDisk:    false,
		SupportsHotplugNetwork: false,
		// PUT /snapshot/create, PUT /snapshot/load.
		SupportsSnapshot: true,
		// FC has no coredump endpoint.
		SupportsCoreDump: false,
		// FC requires raw images.
		SupportsQcow2Disks: false,
		// SendCtrlAltDel is x86_64-only on FC.
		SupportsCtrlAltDel: goruntime.GOARCH == "amd64",
	}
}

// ----------------------------------------------------------------------------
// Lifecycle
// ----------------------------------------------------------------------------

// Boot spawns the firecracker process inside the sandbox netns and PUTs the
// full configuration before issuing InstanceStart. Idempotent on cold
// restart: removes any stale management/vsock socket first.
func (b *FCBackend) Boot(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("FC: Sandbox Boot")()

	if cfg.Sandbox.DiskFormat != "" && cfg.Sandbox.DiskFormat != "raw" {
		return fmt.Errorf("firecracker requires SANDBOX_DISK_FORMAT=raw (got %q)", cfg.Sandbox.DiskFormat)
	}

	overlayPath, _ = filepath.Abs(overlayPath)

	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)
	metricsPath := getFCMetricsPath(spec.ID)

	// Idempotent cleanup: remove stale sockets / metrics from a prior FC.
	_ = os.Remove(socketPath)
	_ = os.Remove(vsockPath)
	_ = os.Remove(metricsPath)

	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return fmt.Errorf("create instance dir: %w", err)
	}

	fcBinary := strings.TrimSpace(cfg.Hypervisor.FCPath)
	if fcBinary == "" {
		return fmt.Errorf("FC_PATH is not set")
	}

	// 1. Spawn firecracker inside the sandbox network namespace.
	args := []string{
		"netns", "exec", spec.NetNSName,
		fcBinary,
		"--api-sock", socketPath,
		"--id", spec.ID,
	}
	if cfg.Hypervisor.FCSeccompPath != "" {
		args = append(args, "--seccomp-filter", cfg.Hypervisor.FCSeccompPath)
	}
	// Note: --no-seccomp / --seccomp-level are deprecated on recent FC; we
	// rely on the bundled default filter unless an explicit path is provided.

	logFile, _ := os.Create(logPath)
	cmd := exec.Command("ip", args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	fmt.Printf(">> [Native] Spawning firecracker inside NetNS %s...\n", spec.NetNSName)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("firecracker start failed: %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("write pid: %w", err)
	}
	cmd.Process.Release()

	// 2. Wait for the API socket to come up.
	client := NewFCClient(socketPath)
	if err := client.WaitForSocket(2 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		_ = b.killPID(pid)
		return fmt.Errorf("FC API socket never appeared. Logs:\n%s", string(logs))
	}

	// 3. Sequenced configuration via PUT.
	apiCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Firecracker's PUT /logger and PUT /metrics open the configured paths
	// for append; on some FC versions they refuse to create the file. Touch
	// both ahead of time so the API call always finds a writable target.
	if err := touchFile(logPath); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("create log path: %w", err)
	}
	if err := touchFile(metricsPath); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("create metrics path: %w", err)
	}

	if err := client.PutLogger(apiCtx, &FCLogger{LogPath: logPath, Level: "Info"}); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /logger: %w", err)
	}
	if err := client.PutMetrics(apiCtx, &FCMetrics{MetricsPath: metricsPath}); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /metrics: %w", err)
	}

	cmdLine := strings.TrimSpace(cfg.Sandbox.KernelCmdline)
	bootSrc := &FCBootSource{
		KernelImagePath: cfg.Paths.KernelPath,
		BootArgs:        cmdLine,
	}
	if cfg.Paths.InitrdPath != "" {
		bootSrc.InitrdPath = cfg.Paths.InitrdPath
	}
	if err := client.PutBootSource(apiCtx, bootSrc); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /boot-source: %w", err)
	}

	if err := client.PutMachineConfig(apiCtx, &FCMachineConfig{
		VcpuCount: spec.CPUs,
		MemSizeMib: spec.MemoryMB,
		Smt:        false,
	}); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /machine-config: %w", err)
	}

	if err := client.PutDrive(apiCtx, "rootfs", &FCDrive{
		DriveID:      "rootfs",
		PathOnHost:   overlayPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /drives/rootfs: %w", err)
	}

	if err := client.PutNetworkInterface(apiCtx, "eth0", &FCNetworkInterface{
		IfaceID:     "eth0",
		HostDevName: spec.TapName,
		GuestMac:    spec.MacAddress,
	}); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /network-interfaces/eth0: %w", err)
	}

	if err := client.PutVsock(apiCtx, &FCVsock{
		GuestCid: getCidFromIP(spec.IPAddress),
		UdsPath:  vsockPath,
	}); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /vsock: %w", err)
	}

	if err := client.PutAction(apiCtx, &FCInstanceActionInfo{ActionType: "InstanceStart"}); err != nil {
		_ = b.killPID(pid)
		return fmt.Errorf("PUT /actions InstanceStart: %w", err)
	}

	fmt.Printf("   [+] FC VM Active! PID: %d, NetNS: %s\n", pid, spec.NetNSName)
	return nil
}

// Start triggers a fresh boot. FC has no notion of a "stopped VM with VMM
// still running", so warm restart collapses into the cold-restart path the
// service layer already exercises when IsSocketAvailable returns false.
//
// When called with a live socket (which only happens for paused VMs in FC),
// we simply Resume.
func (b *FCBackend) Start(ctx context.Context, id string) error {
	if !b.IsSocketAvailable(id) {
		return fmt.Errorf("FC API socket not available; cold restart must go through Boot")
	}
	state, err := b.State(ctx, id)
	if err != nil {
		return err
	}
	if state == StatePaused {
		return b.Resume(ctx, id)
	}
	return fmt.Errorf("FC backend cannot warm-start a VM in state %s", state)
}

// Stop performs a graceful shutdown. On x86_64 we use SendCtrlAltDel; on
// other arches FC has no in-band poweroff signal, so we SIGTERM the process.
func (b *FCBackend) Stop(ctx context.Context, id string) error {
	defer util.Track("FC: Sandbox Stop")()

	socketPath := GetSocketPath(id)
	pidPath := GetPIDPath(id)

	if goruntime.GOARCH == "amd64" {
		client := NewFCClient(socketPath)
		if client.IsSocketAvailable() {
			actCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := client.PutAction(actCtx, &FCInstanceActionInfo{ActionType: "SendCtrlAltDel"}); err != nil {
				fmt.Printf("Warning: SendCtrlAltDel failed for %s: %v\n", id, err)
			}
		}
	}

	// Wait briefly for FC to exit on its own; otherwise SIGTERM, then SIGKILL.
	if data, err := os.ReadFile(pidPath); err == nil {
		pid, _ := strconv.Atoi(string(data))
		if pid > 0 {
			waited := waitForProcessExit(pid, 5*time.Second)
			if !waited {
				_ = syscall.Kill(pid, syscall.SIGTERM)
				if !waitForProcessExit(pid, 3*time.Second) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	}

	_ = os.Remove(socketPath)
	fmt.Printf("   [+] FC VM %s Stopped (process exited).\n", id)
	return nil
}

// Pause suspends the VM via PATCH /vm.
func (b *FCBackend) Pause(ctx context.Context, id string) error {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return fmt.Errorf("sandbox not running")
	}
	apiCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return client.PatchVm(apiCtx, &FCVm{State: "Paused"})
}

// Resume returns a paused VM to running.
func (b *FCBackend) Resume(ctx context.Context, id string) error {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return fmt.Errorf("sandbox not running")
	}
	apiCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return client.PatchVm(apiCtx, &FCVm{State: "Resumed"})
}

// Delete tears down the FC process and the sandbox network namespace.
func (b *FCBackend) Delete(ctx context.Context, id, tapName, nsName string) error {
	defer util.Track("FC: Sandbox Delete")()

	pidPath := GetPIDPath(id)
	if data, err := os.ReadFile(pidPath); err == nil {
		pid, _ := strconv.Atoi(string(data))
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			if !waitForProcessExit(pid, 2*time.Second) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = os.Remove(pidPath)
	}

	if nsName != "" {
		if err := DeleteSandboxNetNS(nsName); err != nil {
			fmt.Printf("Warning: DeleteSandboxNetNS failed for %s (ns=%s): %v\n", id, nsName, err)
		}
	} else if tapName != "" {
		_ = DeleteTap(tapName)
	}
	return nil
}

// State maps FC's native states (Not started / Running / Paused) onto the
// normalised set surfaced to callers.
func (b *FCBackend) State(ctx context.Context, id string) (NormalizedState, error) {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return StateStopped, nil
	}
	info, err := client.GetInstanceInfo(ctx)
	if err != nil {
		return StateKilled, err
	}
	switch strings.ToLower(strings.ReplaceAll(info.State, " ", "")) {
	case "running":
		return StateRunning, nil
	case "paused":
		return StatePaused, nil
	case "notstarted":
		return StateCreated, nil
	default:
		return StateUnknown, nil
	}
}

// Info returns FC's GetInstanceInfo as raw JSON for /info-style debugging.
func (b *FCBackend) Info(ctx context.Context, id string) (string, error) {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return "", fmt.Errorf("sandbox not running (socket missing)")
	}
	info, err := client.GetInstanceInfo(ctx)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// IsSocketAvailable returns true when the FC API socket is dial-able.
func (b *FCBackend) IsSocketAvailable(id string) bool {
	return NewFCClient(GetSocketPath(id)).IsSocketAvailable()
}

// EventSource returns a logger-tailing source for this sandbox.
func (b *FCBackend) EventSource(id string) EventSource {
	return &fcEventSource{id: id}
}

// Counters reads the latest line of FC's metrics file and normalises it into
// a CountersSnapshot.
func (b *FCBackend) Counters(ctx context.Context, id string) (*CountersSnapshot, error) {
	path := getFCMetricsPath(id)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read the file and keep the last complete JSON line. FC's metrics file
	// is rewritten atomically each flush in some configurations, but more
	// commonly it's append-only newline-delimited. We handle both.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var lastLine string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lastLine = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read metrics file: %w", err)
	}
	if lastLine == "" {
		return &CountersSnapshot{
			Disks: map[string]DiskCountersSnapshot{},
			Nets:  map[string]NetCountersSnapshot{},
		}, nil
	}

	// FC's metrics structure has many top-level keys. We only care about
	// block_<id> and net_<id> for per-device counters; vmm-level cpu/memory
	// usage isn't reported by FC, so we leave those at zero and let
	// fetchAgentMetrics (the in-guest agent's /metrics) cover them.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lastLine), &raw); err != nil {
		return nil, fmt.Errorf("parse metrics line: %w", err)
	}

	snap := &CountersSnapshot{
		Disks: map[string]DiskCountersSnapshot{},
		Nets:  map[string]NetCountersSnapshot{},
	}
	for key, payload := range raw {
		switch {
		case strings.HasPrefix(key, "block_"):
			var d struct {
				ReadBytes  uint64 `json:"read_bytes"`
				WriteBytes uint64 `json:"write_bytes"`
				ReadCount  uint64 `json:"read_count"`
				WriteCount uint64 `json:"write_count"`
			}
			if err := json.Unmarshal(payload, &d); err == nil {
				snap.Disks["_"+key] = DiskCountersSnapshot{
					ReadBytes:  d.ReadBytes,
					WriteBytes: d.WriteBytes,
					ReadOps:    d.ReadCount,
					WriteOps:   d.WriteCount,
				}
			}
		case strings.HasPrefix(key, "net_"):
			var n struct {
				RxBytes   uint64 `json:"rx_bytes_count"`
				TxBytes   uint64 `json:"tx_bytes_count"`
				RxPackets uint64 `json:"rx_packets_count"`
				TxPackets uint64 `json:"tx_packets_count"`
			}
			if err := json.Unmarshal(payload, &n); err == nil {
				snap.Nets["_"+key] = NetCountersSnapshot{
					RxBytes:  n.RxBytes,
					TxBytes:  n.TxBytes,
					RxFrames: n.RxPackets,
					TxFrames: n.TxPackets,
				}
			}
		}
	}
	return snap, nil
}

func (b *FCBackend) killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if !waitForProcessExit(pid, 1*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// kill(pid, 0) tests existence without sending a signal.
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// getFCMetricsPath returns the on-disk path FC writes its periodic metrics
// snapshots to.
func getFCMetricsPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/fc.metrics.json", InstancesRoot, sbxID)
}

// touchFile creates the file if it does not already exist. The parent
// directory must already exist.
func touchFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	return f.Close()
}

// ----------------------------------------------------------------------------
// FCClient — HTTP-over-Unix-socket client for the Firecracker REST API
// ----------------------------------------------------------------------------

// FCClient is a low-level wrapper around Firecracker's REST API.
type FCClient struct {
	socketPath string
	timeout    time.Duration
	httpClient *http.Client
}

// NewFCClient creates a FCClient with a default 5 s timeout.
func NewFCClient(socketPath string) *FCClient {
	c := &FCClient{
		socketPath: socketPath,
		timeout:    5 * time.Second,
	}
	c.httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		},
		Timeout: c.timeout,
	}
	return c
}

func (c *FCClient) IsSocketAvailable() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c *FCClient) WaitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.IsSocketAvailable() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("FC API socket not available after %s", timeout)
}

func (c *FCClient) do(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	url := "http://localhost" + path
	var reqBody io.Reader
	hasBody := false
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
		hasBody = true
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("FC %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("FC read response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return respBody, nil
	}
	return respBody, fmt.Errorf("FC %s %s -> %d: %s", method, path, resp.StatusCode, respBody)
}

// API helpers --------------------------------------------------------------

func (c *FCClient) PutLogger(ctx context.Context, body *FCLogger) error {
	_, err := c.do(ctx, http.MethodPut, "/logger", body)
	return err
}
func (c *FCClient) PutMetrics(ctx context.Context, body *FCMetrics) error {
	_, err := c.do(ctx, http.MethodPut, "/metrics", body)
	return err
}
func (c *FCClient) PutBootSource(ctx context.Context, body *FCBootSource) error {
	_, err := c.do(ctx, http.MethodPut, "/boot-source", body)
	return err
}
func (c *FCClient) PutMachineConfig(ctx context.Context, body *FCMachineConfig) error {
	_, err := c.do(ctx, http.MethodPut, "/machine-config", body)
	return err
}
func (c *FCClient) PutDrive(ctx context.Context, id string, body *FCDrive) error {
	_, err := c.do(ctx, http.MethodPut, "/drives/"+id, body)
	return err
}
func (c *FCClient) PutNetworkInterface(ctx context.Context, id string, body *FCNetworkInterface) error {
	_, err := c.do(ctx, http.MethodPut, "/network-interfaces/"+id, body)
	return err
}
func (c *FCClient) PutVsock(ctx context.Context, body *FCVsock) error {
	_, err := c.do(ctx, http.MethodPut, "/vsock", body)
	return err
}
func (c *FCClient) PutAction(ctx context.Context, body *FCInstanceActionInfo) error {
	_, err := c.do(ctx, http.MethodPut, "/actions", body)
	return err
}
func (c *FCClient) PatchVm(ctx context.Context, body *FCVm) error {
	_, err := c.do(ctx, http.MethodPatch, "/vm", body)
	return err
}

func (c *FCClient) GetInstanceInfo(ctx context.Context) (*FCInstanceInfo, error) {
	body, err := c.do(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return nil, err
	}
	var info FCInstanceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse instance info: %w", err)
	}
	return &info, nil
}

// ----------------------------------------------------------------------------
// Firecracker request / response DTOs
// ----------------------------------------------------------------------------

// FCBootSource: PUT /boot-source.
type FCBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// FCMachineConfig: PUT /machine-config.
type FCMachineConfig struct {
	VcpuCount       int  `json:"vcpu_count"`
	MemSizeMib      int  `json:"mem_size_mib"`
	Smt             bool `json:"smt,omitempty"`
	TrackDirtyPages bool `json:"track_dirty_pages,omitempty"`
}

// FCDrive: PUT /drives/{drive_id}.
type FCDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	PartUUID     string `json:"partuuid,omitempty"`
}

// FCNetworkInterface: PUT /network-interfaces/{iface_id}.
type FCNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMac    string `json:"guest_mac,omitempty"`
}

// FCVsock: PUT /vsock.
type FCVsock struct {
	GuestCid uint64 `json:"guest_cid"`
	UdsPath  string `json:"uds_path"`
}

// FCLogger: PUT /logger.
type FCLogger struct {
	LogPath       string `json:"log_path"`
	Level         string `json:"level,omitempty"`
	ShowLevel     bool   `json:"show_level,omitempty"`
	ShowLogOrigin bool   `json:"show_log_origin,omitempty"`
}

// FCMetrics: PUT /metrics.
type FCMetrics struct {
	MetricsPath string `json:"metrics_path"`
}

// FCInstanceActionInfo: PUT /actions.
type FCInstanceActionInfo struct {
	ActionType string `json:"action_type"` // InstanceStart | SendCtrlAltDel | FlushMetrics
}

// FCVm: PATCH /vm.
type FCVm struct {
	State string `json:"state"` // Paused | Resumed
}

// FCInstanceInfo is the response body of GET /.
type FCInstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	VmmVersion string `json:"vmm_version"`
	AppName    string `json:"app_name"`
}

// ----------------------------------------------------------------------------
// fcEventSource — tails Firecracker's logger file and emits semantic events
// ----------------------------------------------------------------------------

type fcEventSource struct {
	id string
}

func (s *fcEventSource) Source() string     { return "fc" }
func (s *fcEventSource) OffsetPath() string { return GetEventOffsetPath(s.id) + ".fc" }

func (s *fcEventSource) Poll(ctx context.Context, offset int64) ([]EventRecord, int64, error) {
	path := GetLogPath(s.id)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, fmt.Errorf("open fc log: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seek to %d: %w", offset, err)
	}

	var out []EventRecord
	br := bufio.NewReader(f)
	read := int64(0)
	for {
		line, err := br.ReadString('\n')
		// Only treat full lines (newline-terminated) as committed; partial
		// reads at EOF are ignored so we resume on the next poll.
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[event_monitor] FC log read error for %s at %d: %v", s.id, offset+read, err)
			break
		}
		read += int64(len(line))

		ev, ok := classifyFCLogLine(strings.TrimRight(line, "\n"))
		if !ok {
			continue
		}
		out = append(out, ev)
	}
	return out, offset + read, nil
}

// classifyFCLogLine maps a single Firecracker log line to a semantic event.
// Lines that look like noise (debug-only or unrelated) are dropped.
func classifyFCLogLine(line string) (EventRecord, bool) {
	if line == "" {
		return EventRecord{}, false
	}
	props := map[string]any{"line": line}

	// Extract a level if present (FC default format includes it after the timestamp).
	level := ""
	switch {
	case strings.Contains(line, " ERROR "):
		level = "error"
	case strings.Contains(line, " WARN "):
		level = "warn"
	case strings.Contains(line, " INFO "):
		level = "info"
	}
	if level != "" {
		props["level"] = level
	}

	switch {
	case strings.Contains(line, "InstanceStart") && strings.Contains(line, "successfully"):
		return EventRecord{Event: "vm.boot", Properties: props}, true
	case strings.Contains(line, "SendCtrlAltDel"):
		return EventRecord{Event: "vm.shutdown", Properties: props}, true
	case strings.Contains(line, "VM paused"):
		return EventRecord{Event: "vm.pause", Properties: props}, true
	case strings.Contains(line, "VM resumed"):
		return EventRecord{Event: "vm.resume", Properties: props}, true
	case strings.Contains(line, "Guest-initiated"), strings.Contains(line, "panic"):
		return EventRecord{Event: "guest.panic", Properties: props}, true
	}
	// Surface ERROR/WARN lines as generic logger events; INFO and below are
	// dropped to avoid swamping the events collection.
	if level == "error" || level == "warn" {
		return EventRecord{Event: "logger." + level, Properties: props}, true
	}
	return EventRecord{}, false
}
