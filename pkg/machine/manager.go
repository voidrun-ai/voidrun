package machine

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/network"
	"voidrun/pkg/timer"
)

// Start handles Fresh Boot (API Injection) and Restore (API Restore)
func Start(cfg config.Config, spec model.SandboxSpec, overlayPath string, restorePath string) error {
	defer timer.Track("Sandbox Start (Total)")()
	fmt.Printf("   [CONFIG] Bridge Name: '%s'\n", cfg.Network.BridgeName)
	fmt.Printf("   [CONFIG] TAP Prefix: '%s'\n", cfg.Network.TapPrefix)
	fmt.Printf("   [CONFIG] Instances Dir: '%s'\n", cfg.Paths.InstancesDir)
	overlayPath, _ = filepath.Abs(overlayPath)
	instanceDir := filepath.Dir(overlayPath)
	socketPath := filepath.Join(instanceDir, "vm.sock")
	logPath := filepath.Join(instanceDir, "vm.log")
	pidPath := filepath.Join(instanceDir, "vm.pid")
	tapPath := filepath.Join(instanceDir, "vm.tap")
	vsockPath := filepath.Join(instanceDir, "vsock.sock")

	// Generate MAC based on IP
	macAddr := network.GenerateMAC(spec.IPAddress)
	log.Printf("   [Net] Generated MAC %s for IP %s\n", macAddr, spec.IPAddress)

	// Create TAP interface (Detached state)
	// We do NOT attach to bridge yet to avoid EBUSY errors in CLH
	tapName, err := network.CreateRandomTap(macAddr, cfg.Network.TapPrefix)
	if err != nil {
		return err
	}

	log.Printf("   [Net] Created TAP interface %s\n", tapName)

	// Save TAP name for cleanup later
	os.WriteFile(tapPath, []byte(tapName), 0644)

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

	// Save PID
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0644); err != nil {
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

	// 5. Inject Configuration via API
	if restorePath != "" {
		// === RESTORE MODE ===
		fmt.Printf("   [+] Restoring from snapshot: %s\n", restorePath)
		absRestorePath, _ := filepath.Abs(restorePath)

		// Restore Payload
		restoreConfig := map[string]interface{}{
			"source_url": fmt.Sprintf("file://%s", absRestorePath),
			// We re-attach network config here
			"net": []NetConfig{{Tap: tapName, Mac: macAddr}},
		}

		if err := client.SendJSON("vm.restore", restoreConfig); err != nil {
			Stop(spec.ID)
			return fmt.Errorf("restore API failed: %w", err)
		}

		// Note: The caller (Restore function) usually handles 'resume',
		// but if we are here via direct Start call, we leave it paused
		// or let the caller handle it.

	} else {
		// === FRESH BOOT MODE ===
		fmt.Println("   [+] Injecting Configuration via API...")

		iface := "eth0"
		gateway := cfg.Network.GetCleanGateway()
		netmask := cfg.Network.GetNetmask()
		// envVars := spec.EnvVars

		hostname := "voidrun"

		kernelIPArgs := fmt.Sprintf(
			"ip=%s::%s:%s:%s:%s:off",
			spec.IPAddress,
			gateway,
			netmask,
			hostname,
			iface,
		)

		envVars := ""

		debugConsole := cfg.Sandbox.DebugBootConsole
		if debugConsole {
			log.Printf("   [Boot] Debug console enabled (vm log: %s)", logPath)
		}
		consoleArgs := "console=hvc0"
		if debugConsole {
			consoleArgs = "console=ttyS0 console=hvc0"
		}

		cmdLine := fmt.Sprintf(
			"%s root=/dev/vda rw init=/sbin/init net.ifnames=0 biosdevname=0 %s %s",
			consoleArgs,
			kernelIPArgs,
			envVars,
		)
		log.Printf("   [Kernel] CmdLine: %s\n", cmdLine)

		payload := PayloadConfig{
			Kernel:  cfg.Paths.KernelPath,
			CmdLine: cmdLine,
		}
		if cfg.Paths.InitrdPath != "" {
			initrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
			payload.Initramfs = initrdPath
		}
		log.Printf("   [CLH] Kernel: %s\n", payload.Kernel)
		if payload.Initramfs != "" {
			log.Printf("   [CLH] Initrd: %s\n", payload.Initramfs)
		}
		log.Printf("   [CLH] CmdLine: %s\n", payload.CmdLine)

		// Create Config Struct
		cfg := CLHConfig{
			Payload: payload,
			Cpus: CpusConfig{
				BootVcpus: spec.CPUs,
				MaxVcpus:  spec.CPUs,
			},
			Memory: MemoryConfig{
				Size:      int64(spec.MemoryMB) * 1024 * 1024,
				Shared:    true,
				Mergeable: true,
				Prefault:  false,
			},
			Disks: []DiskConfig{
				{Path: overlayPath},
			},
			// Remove IP from here (Kernel handles it), just pass Layer 2 info
			Net:     []NetConfig{{Tap: tapName, Mac: macAddr}},
			Rng:     RngConfig{Src: "/dev/urandom"},
			Serial:  ConsoleConfig{Mode: func() string {
				if debugConsole {
					return "Tty"
				}
				return "Null"
			}()},
			Console: ConsoleConfig{Mode: func() string {
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

		// A. Send Config
		if err := client.SendJSON("vm.create", cfg); err != nil {
			Stop(spec.ID)
			return fmt.Errorf("vm.create failed: %w", err)
		}

		// B. Send Boot Signal
		fmt.Println("   [+] Sending Boot Signal...")
		if err := client.Send("vm.boot"); err != nil {
			Stop(spec.ID)
			return fmt.Errorf("vm.boot failed: %w", err)
		}
	}

	// ---------------------------------------------------------
	// FIX: Late Binding - Enable Network NOW
	// Cloud Hypervisor has opened the TAP. Now we attach it to the bridge.
	// This avoids the "Device Busy" error during restore.
	// ---------------------------------------------------------
	fmt.Printf("   [Net] Config Bridge Name: %s\n", cfg.Network.BridgeName)
	fmt.Printf("   [Net] Attaching %s to bridge %s...\n", tapName, cfg.Network.BridgeName)
	if err := network.EnableTap(cfg.Network.BridgeName, tapName); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("network attach failed (bridge: %s, tap: %s): %v", cfg.Network.BridgeName, tapName, err)
	}
	// ---------------------------------------------------------

	fmt.Printf("   [+] VM Active! PID: %d, Tap: %s\n", cmd.Process.Pid, tapName)
	return nil
}

// WaitForAgent attempts to connect to the guest agent via VSOCK until it responds or the timeout elapses.
// This is used for synchronous sandbox creation flows when the client requests readiness.

// Stop gracefully kills the VM and cleans up network
func Stop(id string) error {
	instanceDir := GetInstanceDir(id)
	pidPath := GetPIDPath(id)
	tapPath := GetTapPath(id)

	// 1. Kill Process
	data, err := os.ReadFile(pidPath)
	if err == nil {
		pid, _ := strconv.Atoi(string(data))
		if process, err := os.FindProcess(pid); err == nil {
			process.Signal(syscall.SIGTERM)
		}
		os.Remove(pidPath)
	}

	// 2. Clean Network
	if tapData, err := os.ReadFile(tapPath); err == nil {
		tapName := string(tapData)
		network.DeleteTap(tapName)
		os.Remove(tapPath)
	}

	fmt.Printf("   [+] VM %s Stopped.\n", id)
	_ = instanceDir // Suppress unused variable warning
	return nil
}

func VerifyVMNetwork(vmID string) error {
	gatewayIP := "192.168.100.1"

	// 1. CRITICAL: Check Local Connectivity (Fast)
	// We retry this loop because boot takes time.
	localOK := false
	for i := 0; i < 3000; i++ {
		_, err := ExecuteCommand(vmID, "ping", []string{"-c", "1", "-W", "1", gatewayIP})
		if err == nil {
			localOK = true
			break
		}
		// log.Printf("Waiting for VM network to stabilize...")
		time.Sleep(5 * time.Millisecond)
	}

	if !localOK {
		return fmt.Errorf("critical: VM could not reach Host Gateway")
	}
	return nil
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
