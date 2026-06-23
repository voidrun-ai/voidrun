package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"voidrun/pkg/compute"
)

const defaultNetDeviceID = "net0"

func (p *Provider) StartVM(ctx context.Context, cfg compute.VMConfig) error {
	overlayPath, _ := filepath.Abs(cfg.OverlayPath)
	if overlayPath == "" {
		overlayPath = cfg.RootVolume.Path
	}

	socketPath := compute.GetSocketPath(cfg.ID)
	logPath := compute.GetLogPath(cfg.ID)
	pidPath := compute.GetPIDPath(cfg.ID)
	vsockPath := compute.GetVsockPath(cfg.ID)
	eventPath := compute.GetEventPath(cfg.ID)

	host := compute.Host()
	chBinary := host.CHBinary
	if chBinary == "" {
		return fmt.Errorf("cloud_hypervisor: CH binary not configured")
	}

	cmdLine := strings.TrimSpace(cfg.KernelCmdline)
	consoleMode := "off"
	if cfg.DebugConsole {
		consoleMode = "tty"
	}

	imageType := "qcow2"
	backingFiles := "on"
	switch string(cfg.RootVolume.Format) {
	case "raw":
		imageType = "raw"
		backingFiles = "off"
	case "qcow2-flat":
		imageType = "qcow2"
		backingFiles = "off"
	}

	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + eventPath,
		"--kernel", cfg.KernelPath,
		"--cmdline", cmdLine,
		"--cpus", fmt.Sprintf("boot=%d,max=%d", cfg.VCPU, cfg.VCPU),
		"--memory", fmt.Sprintf("size=%dM,shared=on,mergeable=off", cfg.MemMB),
		"--disk", fmt.Sprintf("path=%s,backing_files=%s,image_type=%s", overlayPath, backingFiles, imageType),
		"--net", fmt.Sprintf("tap=%s,mac=%s", cfg.TapName, cfg.MacAddress),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", cidFromIP(cfg.IPAddress), vsockPath),
		"--rng", "src=/dev/urandom",
		"--serial", consoleMode,
		"--console", consoleMode,
	}

	if cfg.InitrdPath != "" {
		initrdPath, _ := filepath.Abs(cfg.InitrdPath)
		args = append(args, "--initramfs", initrdPath)
	}

	if cfg.EnableSecurity {
		args = append(args, "--seccomp", "true", "--landlock")
		args = append(args, "--landlock-rules")
		args = append(args, buildLandlockRules(cfg, overlayPath, logPath)...)
	}

	netnsArgs := append([]string{"netns", "exec", cfg.NetNSName, chBinary}, args...)
	cmd := exec.CommandContext(ctx, "ip", netnsArgs...)

	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("cloud_hypervisor start: %w", err)
	}
	logFile.Close()

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		return err
	}
	cmd.Process.Release()

	client := NewCLHClient(socketPath)
	if err := client.WaitForSocket(2 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		_ = p.StopVM(context.Background(), cfg.ID)
		return fmt.Errorf("VM crashed on start. Logs:\n%s", string(logs))
	}
	return nil
}

func (p *Provider) ColdBoot(ctx context.Context, cfg compute.VMConfig) error {
	return p.startViaAPI(ctx, cfg)
}

func (p *Provider) startViaAPI(ctx context.Context, cfg compute.VMConfig) error {
	overlayPath, _ := filepath.Abs(cfg.OverlayPath)
	socketPath := compute.GetSocketPath(cfg.ID)
	logPath := compute.GetLogPath(cfg.ID)
	pidPath := compute.GetPIDPath(cfg.ID)
	vsockPath := compute.GetVsockPath(cfg.ID)
	eventPath := compute.GetEventPath(cfg.ID)

	host := compute.Host()
	chBinary := host.CHBinary

	args := []string{"--api-socket", socketPath, "--log-file", logPath, "--event-monitor", "path=" + eventPath}
	netnsArgs := append([]string{"netns", "exec", cfg.NetNSName, chBinary}, args...)
	cmd := exec.CommandContext(ctx, "ip", netnsArgs...)

	logFile, _ := os.Create(logPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644)
	cmd.Process.Release()

	client := NewCLHClient(socketPath)
	if err := client.WaitForSocket(2 * time.Second); err != nil {
		return err
	}

	payload := PayloadConfig{Kernel: cfg.KernelPath, Cmdline: strings.TrimSpace(cfg.KernelCmdline)}
	if cfg.InitrdPath != "" {
		payload.Initramfs, _ = filepath.Abs(cfg.InitrdPath)
	}

	vmCfg := VmConfig{
		Payload: &payload,
		Cpus:    &CpusConfig{BootVcpus: cfg.VCPU, MaxVcpus: cfg.VCPU},
		Memory: &MemoryConfig{
			Size: int64(cfg.MemMB) * 1024 * 1024, Shared: true,
		},
		Disks: []DiskConfig{{Path: overlayPath}},
		Net:   []NetConfig{{ID: defaultNetDeviceID, Tap: cfg.TapName, Mac: cfg.MacAddress}},
		Rng:   &RngConfig{Src: "/dev/urandom"},
		Serial: &ConsoleConfig{Mode: consoleMode(cfg.DebugConsole)},
		Console: &ConsoleConfig{Mode: consoleMode(cfg.DebugConsole)},
		Vsock: &VsockConfig{Cid: cidFromIP(cfg.IPAddress), Socket: vsockPath},
	}

	clh := NewCLHClient(socketPath)
	if err := clh.VmCreate(ctx, &vmCfg); err != nil {
		return err
	}
	return clh.VmBoot(ctx)
}

func consoleMode(debug bool) string {
	if debug {
		return "Tty"
	}
	return "Null"
}

func (p *Provider) StopVM(ctx context.Context, id string) error {
	socketPath := compute.GetSocketPath(id)
	client := NewCLHClient(socketPath)
	if client.IsSocketAvailable() {
		if err := client.VmShutdown(ctx); err != nil {
			fmt.Printf("Warning: VmShutdown failed for %s: %v\n", id, err)
		}
	}
	return nil
}

func (p *Provider) StartGuest(ctx context.Context, id string) error {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("VM socket not available")
	}
	state, err := client.GetState(ctx)
	if err != nil {
		return err
	}
	if state != VmStateShutdown && state != "Created" {
		return fmt.Errorf("VM must be shutdown or created to start (state: %s)", state)
	}
	return client.VmBoot(ctx)
}

func (p *Provider) PauseVM(ctx context.Context, id string) error {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("sandbox not running")
	}
	return client.VmPause(ctx)
}

func (p *Provider) ResumeVM(ctx context.Context, id string) error {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("sandbox not running")
	}
	return client.VmResume(ctx)
}

func (p *Provider) DeleteVM(ctx context.Context, id string) error {
	socketPath := compute.GetSocketPath(id)
	pidPath := compute.GetPIDPath(id)

	client := NewCLHClient(socketPath)
	if client.IsSocketAvailable() {
		_ = client.VmDelete(ctx)
	}

	if data, err := os.ReadFile(pidPath); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if process, err := os.FindProcess(pid); err == nil {
			process.Signal(syscall.SIGTERM)
			time.Sleep(100 * time.Millisecond)
		}
		os.Remove(pidPath)
	}
	return nil
}

func (p *Provider) Snapshot(ctx context.Context, id string, snapshotDir string) error {
	socketPath := compute.GetSocketPath(id)
	client := NewCLHClientWithTimeout(socketPath, 30*time.Second)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("sandbox not running")
	}

	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return err
	}

	_ = client.VmPause(ctx)
	url := "file://" + snapshotDir + "/"
	if err := client.VmSnapshot(ctx, url); err != nil {
		return err
	}
	return client.VmmShutdown(ctx)
}

func (p *Provider) Restore(ctx context.Context, cfg compute.VMConfig) error {
	if err := p.startViaAPI(ctx, cfg); err != nil {
		return err
	}
	socketPath := compute.GetSocketPath(cfg.ID)
	client := NewCLHClient(socketPath)
	restoreCfg := &RestoreConfig{
		SourceURL: "file://" + cfg.SnapshotDir + "/",
	}
	if err := client.VmRestore(ctx, restoreCfg); err != nil {
		return err
	}
	return client.VmBoot(ctx)
}

func (p *Provider) GetState(ctx context.Context, id string) (compute.VMState, error) {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return compute.VMStateDead, nil
	}
	state, err := client.GetState(ctx)
	if err != nil {
		return compute.VMStateDead, err
	}
	return normalizeState(state), nil
}

func (p *Provider) Info(ctx context.Context, id string) (json.RawMessage, error) {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return nil, fmt.Errorf("sandbox not running")
	}
	info, err := client.VmInfo(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(info)
}

func (p *Provider) IsAvailable(id string) bool {
	return NewCLHClientForSandbox(id).IsSocketAvailable()
}

func (p *Provider) Counters(ctx context.Context, id string) (json.RawMessage, error) {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return nil, fmt.Errorf("socket unavailable")
	}
	c, err := client.VmCounters(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

func normalizeState(state string) compute.VMState {
	switch strings.ToLower(state) {
	case "running", "runningvirtualized":
		return compute.VMStateRunning
	case "paused":
		return compute.VMStatePaused
	case "shutdown", "loaded", "created":
		return compute.VMStateStopped
	default:
		return compute.VMStateStopped
	}
}

func cidFromIP(ipStr string) uint64 {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0
	}
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint64(ip[3]) + 1000
}

func buildLandlockRules(cfg compute.VMConfig, overlayPath, logPath string) []string {
	host := compute.Host()
	absKernel, _ := filepath.Abs(cfg.KernelPath)
	absBaseDir, _ := filepath.Abs(host.BaseImagesDir)
	absInstanceDir, _ := filepath.Abs(filepath.Dir(overlayPath))

	baseName := cfg.ImageType + "-base.qcow2"
	if idx := strings.Index(cfg.ImageType, ":"); idx != -1 {
		baseName = fmt.Sprintf("%s-%s.qcow2", cfg.ImageType[:idx], cfg.ImageType[idx+1:])
	}
	absBackingFile, _ := filepath.Abs(filepath.Join(absBaseDir, baseName))

	rulesMap := map[string]string{
		absKernel:      "r",
		logPath:        "rw",
		absInstanceDir: "rw",
		"/dev/urandom": "r",
		"/dev/net/tun": "rw",
		"/sys":         "r",
	}
	if cfg.InitrdPath != "" {
		absInitrd, _ := filepath.Abs(cfg.InitrdPath)
		rulesMap[absInitrd] = "r"
	}
	if string(cfg.RootVolume.Format) == "" || string(cfg.RootVolume.Format) == "qcow2" {
		absDataDir, _ := filepath.Abs(filepath.Dir(absBaseDir))
		rulesMap[absDataDir] = "r"
		rulesMap[absBaseDir] = "r"
		rulesMap[absBackingFile] = "r"
	}

	var paths []string
	for p := range rulesMap {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) < len(paths[j]) })

	var rules []string
	for _, p := range paths {
		rules = append(rules, fmt.Sprintf("path=%s,access=%s", p, rulesMap[p]))
	}
	return rules
}
