package machine

import (
	"fmt"
	"voidrun/internal/config"
	"voidrun/internal/model"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Restore creates a new VM from a snapshot
func Restore(cfg config.Config, newID, snapshotPath, ip string, cold bool) error {
	newInstanceDir := GetInstanceDir(newID)
	log.Printf(">> Restoring VM ID: %s from Snapshot: %s\n", newID, snapshotPath)

	if _, err := os.Stat(newInstanceDir); err == nil {
		return fmt.Errorf("VM ID %s already exists", newID)
	}

	if err := os.MkdirAll(newInstanceDir, 0755); err != nil {
		return fmt.Errorf("failed to create instance dir: %w", err)
	}

	// Restore disk
	srcDisk := filepath.Join(snapshotPath, "overlay.qcow2")
	dstDisk := filepath.Join(newInstanceDir, "overlay.qcow2")

	fmt.Println("   [+] Copying Disk...")
	if err := exec.Command("cp", srcDisk, dstDisk).Run(); err != nil {
		os.RemoveAll(newInstanceDir)
		return fmt.Errorf("disk copy failed: %w", err)
	}

	// Logic Branch: Cold vs Live
	var dstState string
	if !cold {
		// Live restore: Copy RAM state
		srcState := filepath.Join(snapshotPath, "state")
		dstState = filepath.Join(newInstanceDir, "snapshot_state")

		fmt.Printf("   [+] Copying RAM State from %s to %s\n", srcState, dstState)
		if err := exec.Command("cp", "-r", srcState, dstState).Run(); err != nil {
			os.RemoveAll(newInstanceDir)
			return fmt.Errorf("state copy failed: %w", err)
		}
	} else {
		fmt.Println("   [+] Cold Boot Mode: Discarding old RAM.")
	}

	// Config
	spec := model.SandboxSpec{
		ID:        newID,
		IPAddress: ip,
		CPUs:      1,
		MemoryMB:  1024,
	}

	// Start process
	if err := Start(cfg, spec, dstDisk, dstState); err != nil {
		return err
	}

	// Send Resume (only for live restore)
	if !cold {
		fmt.Println("   [+] Waiting for socket to resume...")
		client := NewAPIClientForVM(newID)

		if err := client.WaitForSocket(2 * time.Second); err != nil {
			return fmt.Errorf("socket timed out waiting for resume: %w", err)
		}

		// Give API a tiny moment to accept connections
		time.Sleep(5 * time.Millisecond)

		if err := client.Send("vm.resume"); err != nil {
			fmt.Printf("   [!] Resume warning: %v\n", err)
		} else {
			fmt.Println("   [+] VM Resumed!")
		}
	}

	return nil
}
