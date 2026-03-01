package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/network"
	"voidrun/pkg/timer"
)

const defaultNetDeviceID = "net0"

func ConfigureNetwork(cfg config.Config, spec *model.SandboxSpec) error {

	fmt.Printf("   [CONFIG] Bridge Name: '%s'\n", cfg.Network.BridgeName)
	fmt.Printf("   [CONFIG] TAP Prefix: '%s'\n", cfg.Network.TapPrefix)
	fmt.Printf("   [CONFIG] Instances Dir: '%s'\n", cfg.Paths.InstancesDir)

	// Use centralized path helpers
	tapPath := GetTapPath(spec.ID)

	// Generate MAC based on IP
	macAddr := network.GenerateMAC(spec.IPAddress)
	log.Printf("   [Net] Generated MAC %s for IP %s\n", macAddr, spec.IPAddress)

	// Create TAP interface (Detached state)
	// We do NOT attach to bridge yet to avoid EBUSY errors in CLH
	tapName, err := network.CreateRandomTap(macAddr, cfg.Network.TapPrefix)
	if err != nil {
		return err
	}

	spec.TapName = tapName
	spec.MacAddress = macAddr

	log.Printf("   [Net] Created TAP interface %s\n", tapName)

	// Save TAP name for cleanup later
	os.WriteFile(tapPath, []byte(tapName), 0644)

	return nil
}

// Create handles Fresh Boot (API Injection)
func Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer timer.Track("Sandbox Start (Total)")()

	overlayPath, _ = filepath.Abs(overlayPath)

	// Use centralized path helpers
	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)

	// 3. Start "Empty" Cloud Hypervisor Process
	clhPath, _ := exec.LookPath("cloud-hypervisor")
	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
	}

	fmt.Printf(">> [Native] Spawning empty CLH process (API Mode)...\n")
	cmd := exec.Command(clhPath, args...)

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
	// === FRESH BOOT MODE ===
	fmt.Println("   [+] Injecting Configuration via API...")

	debugConsole := cfg.Sandbox.DebugBootConsole

	cmdLine := strings.TrimSpace(cfg.Sandbox.KernelCmdline)
	log.Printf("   [Kernel] CmdLine: %s\n", cmdLine)

	payload := PayloadConfig{
		Kernel:  cfg.Paths.KernelPath,
		Cmdline: cmdLine,
	}
	if cfg.Paths.InitrdPath != "" {
		initrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
		payload.Initramfs = initrdPath
	}
	log.Printf("   [CLH] Kernel: %s\n", payload.Kernel)
	if payload.Initramfs != "" {
		log.Printf("   [CLH] Initrd: %s\n", payload.Initramfs)
	}
	log.Printf("   [CLH] CmdLine: %s\n", payload.Cmdline)

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
	fmt.Println("   [+] Sending Boot Signal...")
	if err := clhClient.VmBoot(ctx); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("vm.boot failed: %w", err)
	}

	if err := network.EnableTap(cfg.Network.BridgeName, tapName); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("network attach failed (bridge: %s, tap: %s): %v", cfg.Network.BridgeName, tapName, err)
	}

	fmt.Printf("   [+] VM Active! PID: %d, Tap: %s\n", pid, tapName)
	return nil
}

// Stop gracefully shuts down the VM via CLH API (keeps hypervisor and network for restart)
func Stop(id string) error {
	defer timer.Track("lifecycle: Sandbox Stop")()
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
	defer timer.Track("lifecycle: Sandbox Start")()
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

// Delete removes the VM via CLH API and cleans up all files
func Delete(id string) error {
	socketPath := GetSocketPath(id)
	pidPath := GetPIDPath(id)
	tapPath := GetTapPath(id)

	// 1. Delete VM via CLH API (this will also shutdown the VM)
	client := NewCLHClient(socketPath)
	if client.IsSocketAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		}
		os.Remove(pidPath)
	}

	// 3. Clean up TAP interface
	if tapData, err := os.ReadFile(tapPath); err == nil {
		tapName := string(tapData)
		network.DeleteTap(tapName)
		os.Remove(tapPath)
	}

	// 4. Delete the instance directory
	instanceDir := GetInstanceDir(id)
	fmt.Printf(">> Deleting instance %s at %s\n", id, instanceDir)

	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("failed to delete directory: %w", err)
	}

	fmt.Printf("   [+] VM %s fully deleted.\n", id)
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
