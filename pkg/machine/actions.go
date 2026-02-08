package machine

import (
	"fmt"
	"os"
)

// Delete stops a VM and removes all its files
func Delete(id string) error {
	// Stop the VM (kill process and delete TAP)
	if err := Stop(id); err != nil {
		// Log the error but continue with directory deletion
		fmt.Printf("Warning: Stop failed for %s: %v\n", id, err)
	}

	// Delete the instance directory
	instanceDir := GetInstanceDir(id)
	fmt.Printf(">> Deleting instance %s at %s\n", id, instanceDir)

	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("failed to delete directory: %w", err)
	}
	return nil
}

// Pause pauses a running VM
func Pause(id string) error {
	client := NewAPIClientForVM(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("VM not running")
	}
	return client.Send("vm.pause")
}

// Resume resumes a paused VM
func Resume(id string) error {
	client := NewAPIClientForVM(id)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("VM not running")
	}
	return client.Send("vm.resume")
}

// Info returns the raw JSON info from Cloud Hypervisor
func Info(id string) (string, error) {
	client := NewAPIClientForVM(id)
	if !client.IsSocketAvailable() {
		return "", fmt.Errorf("VM not running (socket missing)")
	}

	body, err := client.Get("vm.info")
	if err != nil {
		return "", err
	}
	return string(body), nil
}
