package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"voidrun/config"
	"voidrun/model"
	"voidrun/util"
)

const defaultNetDeviceID = "net0"

func ConfigureNetwork(cfg config.Config, spec *model.SandboxSpec) error {
	// Generate MAC based on IP
	macAddr := GenerateMAC(spec.IPAddress)
	log.Printf("   [Net] Generated MAC %s for IP %s\n", macAddr, spec.IPAddress)

	// Create an isolated network namespace with a tap device inside it.
	// This protects the host from VM-based network attacks and is immune
	// to host-level `iptables -F` flushes.
	nsName, tapName, err := CreateSandboxNetNS(cfg.Network.BridgeName, macAddr, cfg.Network.Prefix, cfg.Network.Nameservers)
	if err != nil {
		return fmt.Errorf("create netns: %w", err)
	}

	spec.NetNSName = nsName
	spec.TapName = tapName // always "tap0" inside the netns
	spec.MacAddress = macAddr

	log.Printf("   [Net] Created NetNS %s with TAP %s\n", nsName, tapName)

	return nil
}

// Create handles Fresh Boot (API Injection)
func Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("Sandbox Start (Total)")()

	overlayPath, _ = filepath.Abs(overlayPath)

	// Use centralized path helpers
	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)

	// 3. Start "Empty" Cloud Hypervisor Process
	eventPath := GetEventPath(spec.ID)
	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + eventPath,
	}

	fmt.Printf(">> [Native] Spawning empty CLH process inside NetNS %s...\n", spec.NetNSName)
	// Run cloud-hypervisor inside the sandbox network namespace so it can
	// access tap0, which lives inside that namespace.
	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.CHBinary}, args...)
	cmd := exec.Command("ip", netnsArgs...)

	// Redirect IO
	logFile, _ := os.Create(logPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Daemonize

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process start failed: %v", err)
	}

	// Save PID before releasing process handle
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		return err
	}
	cmd.Process.Release()

	// 4. Wait for Socket to appear
	client := NewAPIClient(socketPath)
	if err := client.WaitForSocket(2 * time.Second); err != nil {
		// Read log for debugging
		logs, _ := os.ReadFile(logPath)
		Stop(spec.ID) // Cleanup
		return fmt.Errorf("VM crashed on start. Logs:\n%s", string(logs))
	}

	// Ensure tap0 is attached to br0 in netns after VMM starts
	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] EnsureTapBridge failed in Create: %v\n", err)
	}

	tapName := spec.TapName
	macAddr := spec.MacAddress
	log.Printf("   [Create] spec.TapName=%q, spec.MacAddress=%q\n", tapName, macAddr)

	// 5. Inject Configuration via API
	fmt.Println("   [+] Injecting Configuration via API...")

	debugConsole := cfg.Sandbox.DebugBootConsole

	cmdLine := strings.TrimSpace(cfg.Sandbox.KernelCmdline)
	// log.Printf("   [Kernel] CmdLine: %s\n", cmdLine)

	payload := PayloadConfig{
		Kernel:  cfg.Paths.KernelPath,
		Cmdline: cmdLine,
	}
	if cfg.Paths.InitrdPath != "" {
		initrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
		payload.Initramfs = initrdPath
	}
	// log.Printf("   [CLH] Kernel: %s\n", payload.Kernel)
	if payload.Initramfs != "" {
		// log.Printf("   [CLH] Initrd: %s\n", payload.Initramfs)
	}
	// log.Printf("   [CLH] CmdLine: %s\n", payload.Cmdline)

	// Create Config Struct
	vmCfg := VmConfig{
		Payload: &payload,
		Cpus: &CpusConfig{
			BootVcpus: spec.CPUs,
			MaxVcpus:  spec.CPUs,
		},
		Memory: &MemoryConfig{
			Size:      int64(spec.MemoryMB) * 1024 * 1024,
			Shared:    false,
			Mergeable: false,
			Prefault:  false,
		},
		Disks: []DiskConfig{
			{Path: overlayPath},
		},
		Net: []NetConfig{{ID: defaultNetDeviceID, Tap: tapName, Mac: macAddr}},
		Rng: &RngConfig{Src: "/dev/urandom"},
		Serial: &ConsoleConfig{Mode: func() string {
			if debugConsole {
				return "Tty"
			}
			return "Null"
		}()},
		Console: &ConsoleConfig{Mode: func() string {
			if debugConsole {
				return "Tty"
			}
			return "Null"
		}()},
		Vsock: &VsockConfig{
			Cid:    getCidFromIP(spec.IPAddress),
			Socket: vsockPath,
		},
	}

	// A. Send Config using new CLHClient
	clhClient := NewCLHClient(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := clhClient.VmCreate(ctx, &vmCfg); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("vm.create failed: %w", err)
	}

	// B. Send Boot Signal
	if err := clhClient.VmBoot(ctx); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("vm.boot failed: %w", err)
	}

	fmt.Printf("   [+] VM Active! PID: %d, NetNS: %s\n", pid, spec.NetNSName)
	return nil
}

// BuildCLIArgs constructs the Cloud Hypervisor CLI arguments from the sandbox configuration
func BuildCLIArgs(cfg config.Config, spec model.SandboxSpec, overlayPath string) []string {
	// Use centralized path helpers
	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)
	eventPath := GetEventPath(spec.ID)

	tapName := spec.TapName
	macAddr := spec.MacAddress

	// 1. Map Configurations to CLI Strings
	cmdLine := strings.TrimSpace(cfg.Sandbox.KernelCmdline)

	consoleMode := "off"
	if cfg.Sandbox.DebugBootConsole {
		consoleMode = "tty"
	}

	imageType := "qcow2"
	backingFiles := "on"
	if cfg.Sandbox.DiskFormat == "raw" {
		imageType = "raw"
		backingFiles = "off"
	} else if cfg.Sandbox.DiskFormat == "qcow2-flat" {
		imageType = "qcow2"
		backingFiles = "off"
	}

	// 2. Build the Base CLI Arguments
	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + eventPath,
		"--kernel", cfg.Paths.KernelPath,
		"--cmdline", cmdLine,
		"--cpus", fmt.Sprintf("boot=%d,max=%d", spec.CPUs, spec.CPUs),
		"--memory", fmt.Sprintf("size=%dM", spec.MemoryMB),
		"--disk", fmt.Sprintf("path=%s,backing_files=%s,image_type=%s", overlayPath, backingFiles, imageType),
		"--net", fmt.Sprintf("tap=%s,mac=%s", tapName, macAddr),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", getCidFromIP(spec.IPAddress), vsockPath),
		"--rng", "src=/dev/urandom",
		"--serial", consoleMode,
		"--console", consoleMode,
	}

	if cfg.Paths.InitrdPath != "" {
		initrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
		args = append(args, "--initramfs", initrdPath)
	}

	// 3. Build Dynamic Landlock Rules
	if cfg.Sandbox.Seccomp {
		args = append(args, "--seccomp", "true")
		args = append(args, "--landlock")

		absKernel, _ := filepath.Abs(cfg.Paths.KernelPath)
		absBaseDir, _ := filepath.Abs(cfg.Paths.BaseImagesDir)
		absInstanceDir, _ := filepath.Abs(filepath.Dir(overlayPath))

		// Derive backing file path the same way disk.go does
		baseName := spec.Type + "-base.qcow2"
		if idx := strings.Index(spec.Type, ":"); idx != -1 {
			name := spec.Type[:idx]
			tag := spec.Type[idx+1:]
			baseName = fmt.Sprintf("%s-%s.qcow2", name, tag)
		}
		absBackingFile, _ := filepath.Abs(filepath.Join(absBaseDir, baseName))

		var llRules []string

		// Use a map to collect unique rules, then we'll sort them
		rulesMap := make(map[string]string)

		rulesMap[absKernel] = "r"
		rulesMap[logPath] = "rw"
		rulesMap[absInstanceDir] = "rw"
		rulesMap["/dev/urandom"] = "r"
		rulesMap["/dev/net/tun"] = "rw"
		rulesMap["/sys"] = "r"

		if cfg.Paths.InitrdPath != "" {
			absInitrd, _ := filepath.Abs(cfg.Paths.InitrdPath)
			rulesMap[absInitrd] = "r"
		}

		if backingFiles == "on" {
			absDataDir, _ := filepath.Abs(filepath.Dir(absBaseDir))
			rulesMap[absDataDir] = "r"
			rulesMap[absBaseDir] = "r"
			rulesMap[absBackingFile] = "r"
		}

		var paths []string
		for p := range rulesMap {
			paths = append(paths, p)
		}
		sort.Slice(paths, func(i, j int) bool {
			return len(paths[i]) < len(paths[j])
		})

		for _, p := range paths {
			llRules = append(llRules, fmt.Sprintf("path=%s,access=%s", p, rulesMap[p]))
		}

		args = append(args, "--landlock-rules")
		args = append(args, llRules...)
	}

	return args
}

func CreateCLI(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("Sandbox Start (Total CLI)")()

	overlayPath, _ = filepath.Abs(overlayPath)

	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)

	args := BuildCLIArgs(cfg, spec, overlayPath)
	log.Println(args)

	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.CHBinary}, args...)

	// 4. Start Cloud Hypervisor Process inside the sandbox NetNS
	fmt.Printf(">> [Native] Spawning full CLH process inside NetNS %s (CLI Mode)...\n", spec.NetNSName)
	cmd := exec.Command("ip", netnsArgs...)

	logFile, _ := os.Create(logPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Daemonize

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process start failed: %v", err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		return err
	}
	cmd.Process.Release()

	// 5. Wait for Socket (Acts as a Readiness Probe)
	client := NewAPIClient(socketPath)
	if err := client.WaitForSocket(2 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		Stop(spec.ID)
		return fmt.Errorf("VM crashed on start. Logs:\n%s", string(logs))
	}

	// Ensure tap0 is attached to br0 in netns after VMM starts
	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] EnsureTapBridge failed in CreateCLI: %v\n", err)
	}

	fmt.Printf("   [+] VM Active! PID: %d, NetNS: %s\n", pid, spec.NetNSName)
	return nil
}

// Snapshot creates a snapshot of the VM and terminates the hypervisor.
// It is safe to call concurrently for different sandbox IDs.
func Snapshot(id string) error {
	defer util.Track("lifecycle: Sandbox Snapshot")()
	socketPath := GetSocketPath(id)
	baseSnapshotDir := GetSnapshotBaseDir(id)

	// Generate a unique timestamped directory for this snapshot
	snapshotDir := filepath.Join(baseSnapshotDir, fmt.Sprintf("snap-%d", time.Now().UnixNano()))

	client := NewCLHClientWithTimeout(socketPath, 30*time.Second)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("Sandbox not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ensure base directory exists
	if err := os.MkdirAll(baseSnapshotDir, 0755); err != nil {
		return fmt.Errorf("failed to create snapshot base dir: %w", err)
	}
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("failed to create snapshot dir: %w", err)
	}

	// 1. Pause VM (tolerate InvalidStateTransition — VM may already be paused)
	if err := client.VmPause(ctx); err != nil {
		log.Printf("[Snapshot] Warning: VmPause failed for %s (may already be paused): %v", id, err)
	}

	// 2. Take Snapshot
	snapshotUrl := "file://" + snapshotDir + "/"
	if err := client.VmSnapshot(ctx, snapshotUrl); err != nil {
		return fmt.Errorf("VmSnapshot failed: %w", err)
	}

	// 3. Shutdown VMM (kills the process)
	if err := client.VmmShutdown(ctx); err != nil {
		log.Printf("[Snapshot] Warning: VmmShutdown failed for %s: %v", id, err)
	}

	// 4. Wait for socket to disappear (process dead) — synchronous so the caller
	// knows the VMM is truly gone before DB state is written, and so that old-
	// snapshot cleanup doesn't race with a concurrent Restore's GetLatestSnapshotDir.
	for i := 0; i < 20; i++ {
		if !client.IsSocketAvailable() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if client.IsSocketAvailable() {
		log.Printf("[Snapshot] WARNING: VMM %s still alive after 2s, force-killing", id)
		if err := forceKillByPIDFile(id); err != nil {
			os.Remove(socketPath)
			return fmt.Errorf("VMM %s hung and force-kill failed: %w", id, err)
		}
	}

	os.Remove(socketPath)
	log.Printf("[Snapshot] VM %s snapshotted successfully to %s", id, snapshotDir)

	// 5. Clean up older snapshots synchronously to avoid racing with Restore's
	// GetLatestSnapshotDir. Best-effort: log failures but don't fail the snapshot.
	if entries, err := os.ReadDir(baseSnapshotDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "snap-") {
				fullPath := filepath.Join(baseSnapshotDir, entry.Name())
				if fullPath != snapshotDir {
					if rmErr := os.RemoveAll(fullPath); rmErr != nil {
						log.Printf("[Snapshot] Warning: failed to remove old snapshot %s: %v", fullPath, rmErr)
					}
				}
			}
		}
	} else {
		log.Printf("[Snapshot] Warning: could not read snapshot dir for cleanup %s: %v", baseSnapshotDir, err)
	}

	return nil
}

// Stop gracefully shuts down a VM process via the API and waits for the socket to disappear.
// This is used for cleanup when VM creation/boot fails.
func Stop(id string) error {
	defer util.Track("lifecycle: Sandbox Stop")()
	socketPath := GetSocketPath(id)

	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("Sandbox not running")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.VmmShutdown(ctx); err != nil {
		log.Printf("[Stop] Warning: VmmShutdown failed for %s: %v", id, err)
	}

	for i := 0; i < 20; i++ {
		if !client.IsSocketAvailable() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if client.IsSocketAvailable() {
		log.Printf("[Stop] WARNING: VMM %s still alive after 2s, force-killing", id)
		if err := forceKillByPIDFile(id); err != nil {
			os.Remove(socketPath)
			return fmt.Errorf("VMM %s hung and force-kill failed: %w", id, err)
		}
	}

	os.Remove(socketPath)

	log.Printf("[Stop] VM %s stopped successfully", id)
	return nil
}

// forceKillByPIDFile reads the PID file and forcefully kills the process if it's still alive.
func forceKillByPIDFile(id string) error {
	pidPath := GetPIDPath(id)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("failed to read PID file: %w", err)
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return fmt.Errorf("invalid PID in file: %w", err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return nil // Process already gone
	}

	if err := process.Signal(syscall.SIGKILL); err != nil {
		log.Printf("Warning: failed to send SIGKILL to PID %d: %v", pid, err)
	}

	time.Sleep(200 * time.Millisecond)

	// Check if it's still alive. A zombie process will respond to Signal(0),
	// so we must read its state from /proc to see if it's actually dead.
	if err := process.Signal(syscall.Signal(0)); err == nil {
		statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil {
			fields := strings.Fields(string(statData))
			if len(fields) >= 3 {
				state := fields[2]
				if state == "Z" || state == "X" {
					// It's a zombie, so it's dead
					return nil
				}
			}
		}
		return fmt.Errorf("process %d still alive after SIGKILL", pid)
	}

	return nil
}

// Restore restores a VM from a snapshot using the REST API (to prevent warm boot)
func Restore(cfg config.Config, spec model.SandboxSpec, overlayPath, snapshotDir string) error {
	defer util.Track("lifecycle: Sandbox Restore (API)")()

	if err := EnsureSandboxNetNS(cfg, &spec); err != nil {
		return fmt.Errorf("ensure netns: %w", err)
	}

	overlayPath, _ = filepath.Abs(overlayPath)

	socketPath := GetSocketPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	logPath := GetLogPath(spec.ID)

	// Clean up old socket, vsock, and event files if they exist so we start fresh
	os.Remove(socketPath)
	os.Remove(GetEventPath(spec.ID))
	os.Remove(GetEventOffsetPath(spec.ID))
	os.Remove(GetVsockPath(spec.ID))

	// 1. Build CLI args to start an empty Cloud Hypervisor process
	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + GetEventPath(spec.ID),
	}

	if cfg.Sandbox.Seccomp {
		args = append(args, "--seccomp", "true")
	}

	// 2. Prepend NetNS execution
	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.CHBinary}, args...)

	// 3. Start Cloud Hypervisor Process
	fmt.Printf(">> [Native] Spawning empty CLH process for restore of %s inside NetNS %s...\n", spec.ID, spec.NetNSName)
	cmd := exec.Command("ip", netnsArgs...)

	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Daemonize

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process start failed during restore: %v", err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		return err
	}
	cmd.Process.Release()

	// 4. Wait for Socket to appear
	client := NewAPIClient(socketPath)
	if err := client.WaitForSocket(2 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		Stop(spec.ID) // Cleanup
		return fmt.Errorf("VM crashed on restore startup. Logs:\n%s", string(logs))
	}

	// Ensure tap0 is attached to br0 in netns after VMM starts
	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] EnsureTapBridge failed in Restore: %v\n", err)
	}

	// 5. Send Restore Config via API (use a longer timeout since loading snapshot RAM can take time)
	clhClient := NewCLHClientWithTimeout(socketPath, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sourceURL := "file://" + snapshotDir
	if !strings.HasSuffix(sourceURL, "/") {
		sourceURL += "/"
	}

	restoreCfg := &RestoreConfig{
		SourceURL: sourceURL,
		Prefault:  false,
		Resume:    true,
	}

	if err := clhClient.VmRestore(ctx, restoreCfg); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("vm.restore failed: %w", err)
	}

	fmt.Printf("   [+] VM %s Restored via API! PID: %d\n", spec.ID, pid)
	return nil
}

// RestoreCLI restores a VM from a snapshot
func RestoreCLI(cfg config.Config, spec model.SandboxSpec, overlayPath, snapshotDir string) error {
	defer util.Track("lifecycle: Sandbox Restore")()

	if err := EnsureSandboxNetNS(cfg, &spec); err != nil {
		return fmt.Errorf("ensure netns: %w", err)
	}

	overlayPath, _ = filepath.Abs(overlayPath)

	socketPath := GetSocketPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	logPath := GetLogPath(spec.ID)

	// Clean up old socket, vsock, and event files if they exist so we start fresh
	os.Remove(socketPath)
	os.Remove(GetEventPath(spec.ID))
	os.Remove(GetEventOffsetPath(spec.ID))
	os.Remove(GetVsockPath(spec.ID))

	// 1. Build minimal CLI args for restore
	// CLH v52+ requires --kernel (or --firmware) even when restoring from a snapshot.
	absKernelPath, _ := filepath.Abs(cfg.Paths.KernelPath)
	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + GetEventPath(spec.ID),
		"--kernel", absKernelPath,
	}

	if cfg.Paths.InitrdPath != "" {
		absInitrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
		args = append(args, "--initramfs", absInitrdPath)
	}

	if cfg.Sandbox.Seccomp {
		args = append(args, "--seccomp", "true")
		args = append(args, "--landlock")

		absBaseDir, _ := filepath.Abs(cfg.Paths.BaseImagesDir)
		// Parent of base-images dir (e.g. /root/void-run-prod) — mirrors the
		// broad read rule used at fresh-boot so CLH can reach all required files.
		absBaseParentDir := filepath.Dir(absBaseDir)
		absInstanceDir, _ := filepath.Abs(filepath.Dir(overlayPath))
		absSnapshotDir, _ := filepath.Abs(snapshotDir)

		// Each rule must be a separate element — CLH's clap parser treats
		// --landlock-rules as a multi-value flag, not a single space-joined string.
		llRules := []string{
			"path=/sys,access=r",
			"path=/dev/urandom,access=r",
			"path=/dev/net/tun,access=rw",
			fmt.Sprintf("path=%s,access=r", absBaseParentDir),
			fmt.Sprintf("path=%s,access=r", absBaseDir),
			fmt.Sprintf("path=%s,access=r", absKernelPath),
			fmt.Sprintf("path=%s,access=rw", absInstanceDir),
			fmt.Sprintf("path=%s,access=r", absSnapshotDir),
		}
		if cfg.Paths.InitrdPath != "" {
			absInitrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
			llRules = append(llRules, fmt.Sprintf("path=%s,access=r", absInitrdPath))
		}
		args = append(args, "--landlock-rules")
		args = append(args, llRules...)
	}

	// 2. Append restore arguments
	restoreArg := fmt.Sprintf("source_url=file://%s/,memory_restore_mode=ondemand,prefault=off,resume=true", snapshotDir)
	args = append(args, "--restore", restoreArg)

	// 3. Prepend NetNS execution
	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.CHBinary}, args...)

	// 4. Start Cloud Hypervisor Process
	fmt.Printf(">> [Native] Spawning restored CLH process for %s inside NetNS %s...\n", spec.ID, spec.NetNSName)
	cmd := exec.Command("ip", netnsArgs...)

	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Daemonize

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process start failed during restore: %v", err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		return err
	}
	cmd.Process.Release()

	// 5. Quick sanity check — just verify the process is alive, don't block
	//    waiting for the CLH API socket. The caller polls the vsock directly
	//    via waitForAgent, which is the actual readiness signal.
	time.Sleep(5 * time.Millisecond)
	if proc, err := os.FindProcess(pid); err == nil {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			logs, _ := os.ReadFile(logPath)
			return fmt.Errorf("VM crashed on restore. Logs:\n%s", string(logs))
		}
	}

	// Ensure tap0 is attached to br0 in netns after VMM starts/restores
	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] EnsureTapBridge failed in Restore: %v\n", err)
	}



	fmt.Printf("   [+] VM %s Restored! PID: %d\n", spec.ID, pid)
	return nil
}

// Delete shuts down and kills the VM process, but leaves the files on disk for the monitor to sync.
func Delete(id, tapName, nsName string) error {
	socketPath := GetSocketPath(id)
	pidPath := GetPIDPath(id)

	// 1. Delete VM via CLH API (this will also shutdown the VM)
	client := NewCLHClient(socketPath)
	if client.IsSocketAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := client.VmDelete(ctx); err != nil {
			fmt.Printf("Warning: VmDelete failed for %s: %v\n", id, err)
		}
	}

	// 2. Kill the CLH process
	if data, err := os.ReadFile(pidPath); err == nil {
		pid, _ := strconv.Atoi(string(data))
		if process, err := os.FindProcess(pid); err == nil {
			process.Signal(syscall.SIGTERM)
			time.Sleep(100 * time.Millisecond)
		}
		os.Remove(pidPath)
	}

	// 3. Destroy the network namespace.
	// This atomically removes: tap0, br0, veth pair, and all iptables rules inside.
	// If nsName is empty (legacy sandbox), fall back to deleting the tap by name.
	if nsName != "" {
		if err := DeleteSandboxNetNS(nsName); err != nil {
			fmt.Printf("Warning: DeleteSandboxNetNS failed for %s (ns=%s): %v\n", id, nsName, err)
		}
	} else if tapName != "" {
		DeleteTap(tapName) // legacy fallback
	}

	return nil
}

// Cleanup removes all files from disk for the given sandbox.
func Cleanup(id string) error {
	// 4. Delete the instance directory
	instanceDir := GetInstanceDir(id)
	fmt.Printf(">> Deleting instance directory %s\n", instanceDir)

	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("failed to delete directory: %w", err)
	}

	fmt.Printf("   [+] VM %s files cleaned up.\n", id)
	return nil
}

// Info returns the raw JSON info from Cloud Hypervisor
func Info(id string) (string, error) {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return "", fmt.Errorf("Sandbox not running (socket missing)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := client.VmInfo(ctx)
	if err != nil {
		return "", err
	}

	// Convert to JSON string for backward compatibility
	jsonBytes, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("failed to marshal info: %w", err)
	}
	return string(jsonBytes), nil
}

// getCidFromIP generates a CID from an IP address for vsock
func getCidFromIP(ipStr string) uint64 {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0
	}
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	// Take the last byte and add offset (3 is minimum, 1000 is safe)
	return uint64(ip[3]) + 1000
}
