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
	"voidrun/util"
)

// FCRuntime implements HypervisorRuntime for Firecracker microVMs.
//
// Key differences from Cloud Hypervisor:
//   - Firecracker only supports raw disk images; qcow2 is not supported.
//   - There is no persistent "stopped-but-alive" state: once the VM guest
//     shuts down the Firecracker process exits, so every restart is a cold boot.
//   - Vsock is exposed differently: the host-visible socket is a per-port file
//     at <vsock.sock>_<port> rather than a single multiplexed socket.
//   - Firecracker has no vm.delete API; termination is handled by SIGTERM.
//   - Pause/resume are supported via PATCH /vm {"state":"Paused|Resumed"}.
type FCRuntime struct{}

// NewFCRuntime returns a FCRuntime ready for use.
func NewFCRuntime() *FCRuntime {
	return &FCRuntime{}
}

// Type returns the hypervisor identifier.
func (r *FCRuntime) Type() HypervisorType {
	return HypervisorFirecracker
}

// SupportsPause returns true; Firecracker v1.x supports pause/resume via PATCH /vm.
func (r *FCRuntime) SupportsPause() bool {
	return true
}

// IsSocketAvailable reports whether the Firecracker API socket file is present.
func (r *FCRuntime) IsSocketAvailable(id string) bool {
	return NewFCClient(GetSocketPath(id)).IsSocketAvailable()
}

// ---------------------------------------------------------------------------
// Create — cold boot
// ---------------------------------------------------------------------------

// Create spawns a new Firecracker process, injects the full VM configuration
// via the REST API, and boots the guest.
//
// Disk notes: Firecracker only supports raw images.  PrepareStorage must be
// called with format "raw" before Create; overlayPath must point to a raw file.
//
// Vsock notes: the CID is derived from the last IP octet, identical to CLH,
// but the host-side socket multiplexing protocol is different (see DialVsockFC).
func (r *FCRuntime) Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("FC: Sandbox Create (Total)")()

	overlayPath, _ = filepath.Abs(overlayPath)

	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)

	// Ensure the instance directory exists (PrepareStorage creates it but be safe).
	instanceDir := GetInstanceDir(spec.ID)
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return fmt.Errorf("fc: mkdir instance dir: %w", err)
	}

	// 1. Spawn the Firecracker process inside the sandbox network namespace.
	//    --api-sock   : path for the management REST socket
	//    --id         : VM identifier (informational)
	//    --log-path   : path for VM log output (written by the logger PUT below)
	fcArgs := []string{
		"--api-sock", socketPath,
		"--id", spec.ID,
	}

	if cfg.FirecrackerConfig.JailerEnabled {
		// When the jailer is in use we wrap the firecracker binary through it.
		// The jailer takes over spawning firecracker in a chroot+namespace.
		fcArgs = buildJailerArgs(cfg, spec, fcArgs)
	}

	log.Printf(">> [FC] Spawning Firecracker inside NetNS %s...\n", spec.NetNSName)
	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.FCBinary}, fcArgs...)

	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("fc: create log file: %w", err)
	}

	cmd := buildCmd("ip", netnsArgs, logFile)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("fc: process start failed: %w", err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		logFile.Close()
		return fmt.Errorf("fc: write pid file: %w", err)
	}
	cmd.Process.Release()

	// 2. Wait for the API socket to appear (Firecracker creates it on startup).
	client := NewFCClient(socketPath)
	if err := client.WaitForSocket(3 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		r.stopByPIDFile(spec.ID)
		return fmt.Errorf("fc: socket timeout. Logs:\n%s", string(logs))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 3. Configure logger (optional but recommended).
	_ = client.PutLogger(ctx, &FCLogger{
		LogPath: logPath,
		Level:   "Warning",
	})

	// 4. Machine config.
	if err := client.PutMachineConfig(ctx, &FCMachineConfig{
		VcpuCount:  spec.CPUs,
		MemSizeMib: spec.MemoryMB,
	}); err != nil {
		r.stopByPIDFile(spec.ID)
		return fmt.Errorf("fc: machine-config: %w", err)
	}

	// 5. Boot source (kernel + cmdline).
	kernelPath, _ := filepath.Abs(cfg.Paths.KernelPath)
	bootArgs := buildFCBootArgs(cfg)

	bs := &FCBootSource{
		KernelImagePath: kernelPath,
		BootArgs:        bootArgs,
	}
	if cfg.Paths.InitrdPath != "" {
		abs, _ := filepath.Abs(cfg.Paths.InitrdPath)
		bs.InitrdPath = abs
	}

	if err := client.PutBootSource(ctx, bs); err != nil {
		r.stopByPIDFile(spec.ID)
		return fmt.Errorf("fc: boot-source: %w", err)
	}

	// 6. Root drive (Firecracker requires raw images).
	if err := client.PutDrive(ctx, &FCDrive{
		DriveID:      "rootfs",
		PathOnHost:   overlayPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		r.stopByPIDFile(spec.ID)
		return fmt.Errorf("fc: drive rootfs: %w", err)
	}

	// 7. Network interface.
	if err := client.PutNetworkInterface(ctx, &FCNetworkInterface{
		IfaceID:     "eth0",
		HostDevName: spec.TapName,
		GuestMac:    spec.MacAddress,
	}); err != nil {
		r.stopByPIDFile(spec.ID)
		return fmt.Errorf("fc: network-interface: %w", err)
	}

	// 8. Vsock.
	cid := uint32(getCidFromIP(spec.IPAddress))
	if err := client.PutVsock(ctx, &FCVsock{
		VsockID:  "1",
		GuestCID: cid,
		UDSPath:  vsockPath,
	}); err != nil {
		r.stopByPIDFile(spec.ID)
		return fmt.Errorf("fc: vsock: %w", err)
	}

	// 9. Start the VM.
	if err := client.InstanceStart(ctx); err != nil {
		r.stopByPIDFile(spec.ID)
		return fmt.Errorf("fc: instance start: %w", err)
	}

	fmt.Printf("   [FC] VM Active! PID: %d, NetNS: %s\n", pid, spec.NetNSName)
	return nil
}

// WarmStart is not supported by Firecracker.
// Firecracker has no "stopped-but-alive" state; callers must use Create for
// every restart.
func (r *FCRuntime) WarmStart(id string) error {
	return ErrWarmStartNotSupported
}

// ---------------------------------------------------------------------------
// Stop / Delete
// ---------------------------------------------------------------------------

// Stop attempts a graceful shutdown via SendCtrlAltDel, then kills the process
// if the VM does not exit within a short grace period.
//
// Network namespace is kept intact so the sandbox IP can be reused on the next
// cold boot.
func (r *FCRuntime) Stop(id string) error {
	defer util.Track("FC: Sandbox Stop")()

	client := NewFCClient(GetSocketPath(id))
	if client.IsSocketAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Best-effort graceful shutdown; ignore the error (VM may already be off).
		_ = client.SendCtrlAltDel(ctx)
		// Give the guest a moment to flush disk writes.
		time.Sleep(300 * time.Millisecond)
	}

	// Always kill the process — Firecracker does not keep a "stopped" process.
	r.stopByPIDFile(id)
	fmt.Printf("   [FC] VM %s stopped.\n", id)
	return nil
}

// Delete terminates the Firecracker process and destroys the network namespace.
// Disk files are left for the event monitor and Cleanup step.
func (r *FCRuntime) Delete(id, tapName, nsName string) error {
	r.stopByPIDFile(id)

	if nsName != "" {
		if err := DeleteSandboxNetNS(nsName); err != nil {
			fmt.Printf("Warning: FC DeleteSandboxNetNS for %s (ns=%s): %v\n", id, nsName, err)
		}
	} else if tapName != "" {
		DeleteTap(tapName)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Pause / Resume
// ---------------------------------------------------------------------------

// Pause suspends a running Firecracker VM via PATCH /vm {"state":"Paused"}.
func (r *FCRuntime) Pause(id string) error {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return fmt.Errorf("fc: sandbox %s not running (socket missing)", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.PauseVM(ctx)
}

// Resume unsuspends a paused Firecracker VM via PATCH /vm {"state":"Resumed"}.
func (r *FCRuntime) Resume(id string) error {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return fmt.Errorf("fc: sandbox %s not running (socket missing)", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.ResumeVM(ctx)
}

// ---------------------------------------------------------------------------
// State / Info
// ---------------------------------------------------------------------------

// GetState queries the Firecracker REST API and returns a normalised state.
func (r *FCRuntime) GetState(id string) (string, error) {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return "stopped", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	raw, err := client.GetState(ctx)
	if err != nil {
		return "unknown", err
	}
	return normaliseFCState(raw), nil
}

// Info returns a JSON representation of the Firecracker instance info.
func (r *FCRuntime) Info(id string) (string, error) {
	client := NewFCClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return "", fmt.Errorf("fc: sandbox %s not running (socket missing)", id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := client.DescribeInstance(ctx)
	if err != nil {
		return "", err
	}

	b, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("fc info: marshal: %w", err)
	}
	return string(b), nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// normaliseFCState maps Firecracker-specific state strings to the application
// vocabulary: "running", "paused", "stopped", "unknown".
func normaliseFCState(raw string) string {
	switch raw {
	case FCStateRunning:
		return "running"
	case FCStatePaused:
		return "paused"
	case FCStateNotStarted:
		return "stopped"
	default:
		return "unknown"
	}
}

// stopByPIDFile reads the PID file and sends SIGTERM + waits briefly.
func (r *FCRuntime) stopByPIDFile(id string) {
	pidPath := GetPIDPath(id)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGTERM)
		// Wait up to 500 ms for a clean exit before giving up.
		done := make(chan struct{})
		go func() {
			proc.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			_ = proc.Signal(syscall.SIGKILL)
		}
	}
	os.Remove(pidPath)
}

// buildFCBootArgs constructs the kernel command line for Firecracker.
// Firecracker requires console=ttyS0 for serial output; the CLH default uses
// virtio-console which Firecracker does not support.
func buildFCBootArgs(cfg config.Config) string {
	base := strings.TrimSpace(cfg.Sandbox.KernelCmdline)
	// Replace or append console= for serial output on Firecracker.
	if !strings.Contains(base, "console=") {
		base += " console=ttyS0"
	}
	return strings.TrimSpace(base)
}

// buildJailerArgs wraps the Firecracker arguments in the jailer invocation.
// See https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md
func buildJailerArgs(cfg config.Config, spec model.SandboxSpec, fcArgs []string) []string {
	jcfg := cfg.FirecrackerConfig
	// Derive a numeric UID/GID from the sandbox's last IP octet (1000+octet)
	// to give each VM a unique unprivileged identity inside the jailer chroot.
	uidgid := strconv.FormatUint(getCidFromIP(spec.IPAddress), 10)

	jailerArgs := []string{
		jcfg.JailerBinary,
		"--id", spec.ID,
		"--exec-file", cfg.FCBinary,
		"--uid", uidgid,
		"--gid", uidgid,
		"--chroot-base-dir", GetInstanceDir(spec.ID),
		"--netns", "/var/run/netns/" + spec.NetNSName,
		"--",
	}
	return append(jailerArgs, fcArgs...)
}

// buildCmd creates an exec.Cmd with stdout/stderr redirected to logFile and
// a new session (daemonise from the caller's process group).
func buildCmd(binary string, args []string, logFile *os.File) *exec.Cmd {
	cmd := exec.Command(binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
