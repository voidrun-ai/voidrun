package machine

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func CreateSnapshot(sbxID string) error {
	instanceDir := GetInstanceDir(sbxID)
	socketPath := GetSocketPath(sbxID)

	log.Printf(">> Creating snapshot for Sandbox ID: %s\n", sbxID)

	client := NewAPIClient(socketPath)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("Sandbox socket not found. Is Sandbox running?")
	}

	// Check Sandbox state
	state, err := client.GetState()
	log.Printf("   [+] Current State: %s\n", state)
	if err != nil {
		return fmt.Errorf("failed to get Sandbox state: %w", err)
	}

	if state != "Running" && state != "Paused" {
		return fmt.Errorf("cannot snapshot Sandbox in state: %s (Must be Running or Paused)", state)
	}

	// Pause if running
	if state == "Running" {
		if err := client.Send("vm.pause"); err != nil {
			return fmt.Errorf("pause failed: %w", err)
		}
		fmt.Println("   [+] Sandbox Paused")
	}

	// Prepare directories
	timestamp := time.Now().Format("20060102-150405")
	snapDir := filepath.Join(instanceDir, "snapshots", timestamp)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return err
	}

	tempStateDir := filepath.Join(instanceDir, "snapshot_temp")
	os.RemoveAll(tempStateDir)
	os.MkdirAll(tempStateDir, 0755)

	fmt.Printf(">> Snapshotting to %s\n", snapDir)

	// Trigger snapshot
	snapshotPayload := map[string]string{
		"destination_url": fmt.Sprintf("file://%s", tempStateDir),
	}
	if err := client.SendJSON("vm.snapshot", snapshotPayload); err != nil {
		if state == "Running" {
			client.Send("vm.resume")
		}
		return fmt.Errorf("snapshot failed: %w", err)
	}
	fmt.Println("   [+] Memory Dumped")

	// Resume immediately
	if state == "Running" {
		if err := client.Send("vm.resume"); err != nil {
			return fmt.Errorf("resume failed: %w", err)
		}
		fmt.Println("   [+] Sandbox Resumed")
	}

	// Copy disk and finalize in background
	go finalizeSnapshot(instanceDir, snapDir, tempStateDir)

	return nil
}

// finalizeSnapshot copies disk and moves state files in the background
func finalizeSnapshot(instanceDir, snapDir, tempStateDir string) {
	srcDisk := filepath.Join(instanceDir, "overlay.qcow2")
	dstDisk := filepath.Join(snapDir, "overlay.qcow2")

	log.Printf("   [Background] Copying disk to snapshot...\n")
	if err := exec.Command("cp", srcDisk, dstDisk).Run(); err != nil {
		log.Printf("   [ERROR] Disk copy failed: %v\n", err)
		return
	}
	log.Println("   [+] Disk Cloned")

	// Move state files
	finalStateDir := filepath.Join(snapDir, "state")
	if err := os.Rename(tempStateDir, finalStateDir); err != nil {
		log.Printf("   [ERROR] State move failed: %v\n", err)
		return
	}

	// Lock state files (read-only)
	filepath.Walk(finalStateDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		return os.Chmod(path, 0444)
	})

	log.Printf("   [+] Snapshot finalized: %s\n", snapDir)
}
