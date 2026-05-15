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

	// fmt.Printf("   [CONFIG] Bridge Name: '%s'\n", cfg.Network.BridgeName)
	// fmt.Printf("   [CONFIG] TAP Prefix: '%s'\n", cfg.Network.TapPrefix)
	// Generate MAC based on IP
	macAddr := GenerateMAC(spec.IPAddress)
	log.Printf("   [Net] Generated MAC %s for IP %s\n", macAddr, spec.IPAddress)

	// Create TAP interface (Detached state)
	// We do NOT attach to bridge yet to avoid EBUSY errors in CLH
	tapName, err := CreateRandomTap(macAddr, cfg.Network.TapPrefix)
	if err != nil {
		return err
	}

	spec.TapName = tapName
	spec.MacAddress = macAddr

	log.Printf("   [Net] Created TAP interface %s\n", tapName)

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

	fmt.Printf(">> [Native] Spawning empty CLH process (API Mode)...\n")
	cmd := exec.Command(cfg.CHBinary, args...)

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
			Shared:    true,
			Mergeable: true,
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
	// fmt.Println("   [+] Sending Boot Signal...")
	if err := clhClient.VmBoot(ctx); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("vm.boot failed: %w", err)
	}

	if err := EnableTap(cfg.Network.BridgeName, tapName); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("network attach failed (bridge: %s, tap: %s): %v", cfg.Network.BridgeName, tapName, err)
	}

	fmt.Printf("   [+] VM Active! PID: %d, Tap: %s\n", pid, tapName)
	return nil
}

func CreateCLI(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("Sandbox Start (Total CLI)")()

	overlayPath, _ = filepath.Abs(overlayPath)

	// Use centralized path helpers
	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)
	eventPath := GetEventPath(spec.ID)

	tapName := spec.TapName
	macAddr := spec.MacAddress
	log.Printf("   [CreateCLI] spec.TapName=%q, spec.MacAddress=%q\n", tapName, macAddr)

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
		"--api-socket", socketPath, // Still useful for monitoring/poweroff
		"--log-file", logPath,
		"--event-monitor", "path=" + eventPath,
		"--kernel", cfg.Paths.KernelPath,
		"--cmdline", cmdLine,
		"--cpus", fmt.Sprintf("boot=%d,max=%d", spec.CPUs, spec.CPUs),
		"--memory", fmt.Sprintf("size=%dM,shared=on,mergeable=on", spec.MemoryMB),
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
		// Key: path, Value: access string ("r" or "rw")
		rulesMap := make(map[string]string)

		// Kernel image (read file)
		rulesMap[absKernel] = "r"
		// Log file (write)
		rulesMap[logPath] = "rw"
		// Entire instance directory: overlay.qcow2, vm.sock, vsock.sock, vm.evt
		rulesMap[absInstanceDir] = "rw"
		// RNG
		rulesMap["/dev/urandom"] = "r"
		// TUN/TAP and sysfs
		rulesMap["/dev/net/tun"] = "rw"
		rulesMap["/sys"] = "r"

		if cfg.Paths.InitrdPath != "" {
			absInitrd, _ := filepath.Abs(cfg.Paths.InitrdPath)
			rulesMap[absInitrd] = "r"
		}

		if backingFiles == "on" {
			// Landlock path traversal requires every ancestor directory to have ReadDir.
			absDataDir, _ := filepath.Abs(filepath.Dir(absBaseDir))
			rulesMap[absDataDir] = "r"
			rulesMap[absBaseDir] = "r"
			rulesMap[absBackingFile] = "r"
		}

		// Sort rules by path length (shortest first) to ensure broader rules
		// are added before narrower ones. This avoids a Landlock bug where
		// adding a specific file rule before a broad directory rule causes
		// siblings of the specific file to be denied access.
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

	log.Println(args)

	// 4. Start Cloud Hypervisor Process
	fmt.Printf(">> [Native] Spawning full CLH process (CLI Mode)...\n")
	cmd := exec.Command(cfg.CHBinary, args...)

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
	// Because we passed the full config, CH creates the socket and boots immediately.
	client := NewAPIClient(socketPath)
	if err := client.WaitForSocket(2 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		Stop(spec.ID)
		return fmt.Errorf("VM crashed on start. Logs:\n%s", string(logs))
	}

	// 6. Network Attach
	if err := EnableTap(cfg.Network.BridgeName, tapName); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("network attach failed (bridge: %s, tap: %s): %v", cfg.Network.BridgeName, tapName, err)
	}

	fmt.Printf("   [+] VM Active! PID: %d, Tap: %s\n", pid, tapName)
	return nil
}

// Stop gracefully shuts down the VM via CLH API (keeps hypervisor and network for restart)
func Stop(id string) error {
	defer util.Track("lifecycle: Sandbox Stop")()
	socketPath := GetSocketPath(id)

	// 1. Gracefully shutdown VM via CLH API (keeps hypervisor process running)
	client := NewCLHClient(socketPath)
	if client.IsSocketAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := client.VmShutdown(ctx); err != nil {
			fmt.Printf("Warning: VmShutdown failed for %s: %v\n", id, err)
		}
	}
	fmt.Printf("   [+] VM %s Stopped (CLH process and TAP interface preserved).\n", id)
	return nil
}

// Start boots a VM that is in shutdown state
func Start(id string) error {
	defer util.Track("lifecycle: Sandbox Start")()
	socketPath := GetSocketPath(id)

	client := NewCLHClient(socketPath)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("VM socket not available. Is the hypervisor process running?")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check current state
	state, err := client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get VM state: %w", err)
	}

	// Can boot from Created or Shutdown states
	if state != VmStateShutdown && state != "Created" {
		return fmt.Errorf("VM must be in shutdown or created state to start (current state: %s)", state)
	}

	// Boot the VM
	fmt.Printf("   [+] Starting VM %s (state: %s)...\n", id, state)
	if err := client.VmBoot(ctx); err != nil {
		return fmt.Errorf("vm.boot failed: %w", err)
	}

	fmt.Printf("   [+] VM %s Started.\n", id)
	return nil
}

// Delete shuts down and kills the VM process, but leaves the files on disk for the monitor to sync.
func Delete(id, tapName string) error {
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
			// Wait for process to exit
			time.Sleep(100 * time.Millisecond)
		}
		os.Remove(pidPath)
	}

	// 3. Clean up TAP interface
	DeleteTap(tapName)

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

// Pause pauses a running VM
func Pause(id string) error {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("Sandbox not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.VmPause(ctx)
}

// Resume resumes a paused VM
func Resume(id string) error {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("Sandbox not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.VmResume(ctx)
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
