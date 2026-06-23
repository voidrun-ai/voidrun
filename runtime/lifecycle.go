package runtime

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"voidrun/config"
	"voidrun/model"
)

func ConfigureNetwork(cfg config.Config, spec *model.SandboxSpec) error {
	macAddr := GenerateMAC(spec.IPAddress)
	log.Printf("   [Net] Generated MAC %s for IP %s\n", macAddr, spec.IPAddress)

	nsName, tapName, err := CreateSandboxNetNS(cfg.Network.BridgeName, macAddr, cfg.Network.Prefix, cfg.Network.Nameservers)
	if err != nil {
		return fmt.Errorf("create netns: %w", err)
	}

	spec.NetNSName = nsName
	spec.TapName = tapName
	spec.MacAddress = macAddr

	log.Printf("   [Net] Created NetNS %s with TAP %s\n", nsName, tapName)
	return nil
}

// Delete shuts down the VM and destroys networking. Instance files remain for monitor sync.
func Delete(ctx context.Context, id, tapName, nsName, hvType string) error {
	_ = DeleteVM(ctx, id, hvType)

	pidPath := GetPIDPath(id)
	if data, err := os.ReadFile(pidPath); err == nil {
		pid, _ := strconv.Atoi(string(data))
		if process, err := os.FindProcess(pid); err == nil {
			process.Signal(syscall.SIGTERM)
			time.Sleep(100 * time.Millisecond)
		}
		os.Remove(pidPath)
	}

	if nsName != "" {
		if err := DeleteSandboxNetNS(nsName); err != nil {
			fmt.Printf("Warning: DeleteSandboxNetNS failed for %s (ns=%s): %v\n", id, nsName, err)
		}
	} else if tapName != "" {
		DeleteTap(tapName)
	}
	return nil
}

// Cleanup removes all files from disk for the given sandbox.
func Cleanup(id string) error {
	instanceDir := GetInstanceDir(id)
	fmt.Printf(">> Deleting instance directory %s\n", instanceDir)
	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("failed to delete directory: %w", err)
	}
	fmt.Printf("   [+] VM %s files cleaned up.\n", id)
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
		return nil
	}

	if err := process.Signal(syscall.SIGKILL); err != nil {
		log.Printf("Warning: failed to send SIGKILL to PID %d: %v", pid, err)
	}

	time.Sleep(200 * time.Millisecond)

	if err := process.Signal(syscall.Signal(0)); err == nil {
		statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil {
			fields := strings.Fields(string(statData))
			if len(fields) >= 3 {
				state := fields[2]
				if state != "Z" && state != "X" {
					return fmt.Errorf("process %d still alive after SIGKILL (state: %s)", pid, state)
				}
			}
		}
	}

	os.Remove(pidPath)
	return nil
}
