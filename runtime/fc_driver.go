package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"voidrun/config"
	"voidrun/model"
)

// FCDriver implements VMDriver for Firecracker microVMs.
//
// Key behavioural differences from CLH:
//   - No warm restart: the Firecracker process exits when the guest halts.
//     Start() always returns an error; the service falls through to cold restart.
//   - API-only configuration: both CreateCLI and Create follow the same
//     spawn → configure via API → InstanceStart flow.
//   - Raw disk images only: Firecracker does not support qcow2 backing files.
//   - No vm.counters: metrics come exclusively from the guest agent.
//   - No --event-monitor: the CLH event file watcher produces no events for
//     Firecracker instances (it checks for file existence before doing I/O).
type FCDriver struct{}

// Name returns the driver identifier.
func (d *FCDriver) Name() string { return DriverFirecracker }

// CreateCLI for Firecracker follows the same API-based flow as Create because
// Firecracker does not support a single-command "boot with full CLI config"
// mode equivalent to CLH's --kernel / --disk / --net flags.
func (d *FCDriver) CreateCLI(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	return d.createAndBoot(cfg, spec, overlayPath)
}

// Create spawns a Firecracker process, configures it via the REST API, and
// boots the microVM.
func (d *FCDriver) Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	return d.createAndBoot(cfg, spec, overlayPath)
}

// Start is not supported for Firecracker because the process exits when the VM
// halts. The service layer detects that the socket is gone and performs a cold
// restart (Create) instead.
func (d *FCDriver) Start(id string) error {
	return fmt.Errorf("firecracker: warm restart is not supported; use Create for cold restart")
}

// Stop sends an ACPI power-off event to the guest and then kills the
// Firecracker process once the socket disappears.
func (d *FCDriver) Stop(id string) error {
	socketPath := GetFCSocketPath(id)
	client := NewFCClient(socketPath)

	if client.IsSocketAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Best-effort graceful shutdown via ACPI.
		if err := client.SendCtrlAltDel(ctx); err != nil {
			log.Printf("[fc] SendCtrlAltDel for %s failed (continuing): %v", id, err)
		}

		// Wait up to 5 s for the process to exit on its own.
		for i := 0; i < 50; i++ {
			time.Sleep(100 * time.Millisecond)
			if !client.IsSocketAvailable() {
				break
			}
		}
	}

	// Kill the process if it is still running.
	d.killProcess(id)

	fmt.Printf("   [+] FC sandbox %s stopped.\n", id)
	return nil
}

// Pause suspends the microVM via PATCH /vm.
func (d *FCDriver) Pause(id string) error {
	client := NewFCClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("sandbox not running (FC socket unavailable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.PauseVM(ctx)
}

// Resume resumes a paused microVM via PATCH /vm.
func (d *FCDriver) Resume(id string) error {
	client := NewFCClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("sandbox not running (FC socket unavailable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.ResumeVM(ctx)
}

// Delete stops the VM, kills the Firecracker process, and destroys the
// sandbox network namespace.
func (d *FCDriver) Delete(id, tapName, nsName string) error {
	// Attempt graceful stop first.
	_ = d.Stop(id)

	// Ensure the process is dead.
	d.killProcess(id)

	// Destroy the network namespace (removes tap0, veth, iptables rules).
	if nsName != "" {
		if err := DeleteSandboxNetNS(nsName); err != nil {
			fmt.Printf("[fc] Warning: DeleteSandboxNetNS failed for %s (ns=%s): %v\n", id, nsName, err)
		}
	} else if tapName != "" {
		DeleteTap(tapName) // legacy fallback
	}

	return nil
}

// IsSocketAvailable returns true if the Firecracker control socket is
// reachable.
func (d *FCDriver) IsSocketAvailable(id string) bool {
	return NewFCClientForSandbox(id).IsSocketAvailable()
}

// GetStateWithContext queries the Firecracker instance info endpoint and maps
// the FC-specific state string to an application-level state.
func (d *FCDriver) GetStateWithContext(ctx context.Context, id string) (string, error) {
	client := NewFCClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return "stopped", nil
	}

	info, err := client.GetInstanceInfo(ctx)
	if err != nil {
		return "killed", fmt.Errorf("FC state query failed: %w", err)
	}

	switch info.State {
	case FCStateRunning:
		return "running", nil
	case FCStatePaused:
		return "paused", nil
	case FCStateNotStarted:
		return "stopped", nil
	default:
		return "stopped", nil
	}
}

// Info returns a JSON-encoded FCInstanceInfo for debugging purposes.
func (d *FCDriver) Info(id string) (string, error) {
	client := NewFCClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return "", fmt.Errorf("sandbox not running (FC socket missing)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := client.GetInstanceInfo(ctx)
	if err != nil {
		return "", err
	}

	b, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("marshal FC info: %w", err)
	}
	return string(b), nil
}

// SocketPath returns the Firecracker API socket path.
func (d *FCDriver) SocketPath(id string) string { return GetFCSocketPath(id) }

// OverlayPath returns the raw disk image path used by Firecracker.
func (d *FCDriver) OverlayPath(id string) string { return GetRawOverlayPath(id) }

// ============================================================================
// Internal helpers
// ============================================================================

// createAndBoot is the shared implementation for CreateCLI and Create.
func (d *FCDriver) createAndBoot(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	overlayPath, _ = filepath.Abs(overlayPath)

	socketPath := GetFCSocketPath(spec.ID)
	logPath := GetFCLogPath(spec.ID)
	pidPath := GetFCPIDPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)

	// Ensure instance directory exists.
	instanceDir := GetInstanceDir(spec.ID)
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return fmt.Errorf("create instance dir: %w", err)
	}

	// Build Firecracker CLI args.
	args := []string{
		"--api-sock", socketPath,
		"--log-path", logPath,
		"--level", "Info",
	}

	// Spawn Firecracker inside the sandbox netns so it can open tap0.
	fmt.Printf(">> [FC] Spawning Firecracker inside NetNS %s...\n", spec.NetNSName)
	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.FCBinary}, args...)
	cmd := exec.Command("ip", netnsArgs...)

	logFile, _ := os.Create(logPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("FC process start failed: %w", err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		return err
	}
	cmd.Process.Release()

	// Wait for the API socket to appear.
	client := NewFCClient(socketPath)
	if err := client.WaitForSocket(3 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		d.killProcess(spec.ID)
		return fmt.Errorf("FC socket not ready: %w\nLogs:\n%s", err, string(logs))
	}

	// Configure the microVM via the REST API.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := d.configureVM(ctx, client, cfg, spec, overlayPath, vsockPath); err != nil {
		d.killProcess(spec.ID)
		return fmt.Errorf("FC configure failed: %w", err)
	}

	// Boot the microVM.
	if err := client.InstanceStart(ctx); err != nil {
		d.killProcess(spec.ID)
		return fmt.Errorf("FC InstanceStart failed: %w", err)
	}

	fmt.Printf("   [+] FC microVM active! PID: %d, NetNS: %s\n", pid, spec.NetNSName)
	return nil
}

// configureVM sends the full VM configuration to Firecracker via the REST API.
func (d *FCDriver) configureVM(
	ctx context.Context,
	client *FCClient,
	cfg config.Config,
	spec model.SandboxSpec,
	overlayPath, vsockPath string,
) error {
	// Machine configuration (vCPUs + memory).
	if err := client.PutMachineConfig(ctx, &FCMachineConfig{
		VcpuCount:  spec.CPUs,
		MemSizeMib: spec.MemoryMB,
	}); err != nil {
		return fmt.Errorf("PutMachineConfig: %w", err)
	}

	// Boot source (kernel + cmdline).
	kernelPath, _ := filepath.Abs(cfg.Paths.KernelPath)
	bootArgs := buildFCBootArgs(cfg)
	bootSrc := &FCBootSource{
		KernelImagePath: kernelPath,
		BootArgs:        bootArgs,
	}
	if cfg.Paths.InitrdPath != "" {
		initrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
		bootSrc.InitrdPath = initrdPath
	}
	if err := client.PutBootSource(ctx, bootSrc); err != nil {
		return fmt.Errorf("PutBootSource: %w", err)
	}

	// Root drive.
	if err := client.PutDrive(ctx, &FCDrive{
		DriveID:      "rootfs",
		PathOnHost:   overlayPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		return fmt.Errorf("PutDrive rootfs: %w", err)
	}

	// Network interface (TAP device inside the netns).
	if err := client.PutNetworkInterface(ctx, &FCNetworkInterface{
		IfaceID:     "eth0",
		HostDevName: spec.TapName,
		GuestMAC:    spec.MacAddress,
	}); err != nil {
		return fmt.Errorf("PutNetworkInterface: %w", err)
	}

	// Vsock device.
	cid := getCidFromIP(spec.IPAddress)
	if err := client.PutVsock(ctx, &FCVsock{
		GuestCID: cid,
		UDSPath:  vsockPath,
	}); err != nil {
		return fmt.Errorf("PutVsock: %w", err)
	}

	return nil
}

// buildFCBootArgs constructs the kernel command-line string for Firecracker.
// Firecracker exposes a UART serial console (ttyS0); append it when debug
// console is requested, otherwise keep the output silent.
func buildFCBootArgs(cfg config.Config) string {
	base := strings.TrimSpace(cfg.Sandbox.KernelCmdline)
	if cfg.Sandbox.DebugBootConsole {
		if !strings.Contains(base, "console=") {
			base += " console=ttyS0"
		}
	} else {
		// Disable serial console output for production use.
		if !strings.Contains(base, "console=") {
			base += " console=ttyS0 quiet loglevel=0"
		}
	}
	return base
}

// killProcess reads the saved PID and sends SIGTERM, then SIGKILL after a
// brief wait.
func (d *FCDriver) killProcess(id string) {
	pidPath := GetFCPIDPath(id)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	proc.Signal(syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	proc.Signal(syscall.SIGKILL)
	os.Remove(pidPath)
}
