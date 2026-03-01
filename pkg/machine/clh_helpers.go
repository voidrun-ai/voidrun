package machine

import (
	"context"
	"fmt"
	"time"
)

// CreateAndBootVM creates and boots a VM in one operation
func CreateAndBootVM(ctx context.Context, socketPath string, config *VmConfig) error {
	client := NewCLHClient(socketPath)

	if err := client.VmCreate(ctx, config); err != nil {
		return fmt.Errorf("failed to create VM: %w", err)
	}

	if err := client.VmBoot(ctx); err != nil {
		return fmt.Errorf("failed to boot VM: %w", err)
	}

	return nil
}

// WaitForVMState waits for the VM to reach a specific state
func WaitForVMState(ctx context.Context, client *CLHClient, targetState string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(deadline)):
			return fmt.Errorf("timeout waiting for state %s", targetState)
		case <-ticker.C:
			state, err := client.GetState(ctx)
			if err != nil {
				continue // Retry on error
			}
			if state == targetState {
				return nil
			}
		}
	}
}

// ShutdownAndDeleteVM gracefully shuts down and deletes a VM
func ShutdownAndDeleteVM(ctx context.Context, socketPath string) error {
	client := NewCLHClient(socketPath)

	// Try graceful shutdown first
	if err := client.VmShutdown(ctx); err != nil {
		// If shutdown fails, try delete anyway
		fmt.Printf("Warning: graceful shutdown failed: %v\n", err)
	} else {
		// Wait for shutdown state
		if err := WaitForVMState(ctx, client, VmStateShutdown, 30*time.Second); err != nil {
			fmt.Printf("Warning: VM did not reach shutdown state: %v\n", err)
		}
	}

	return client.VmDelete(ctx)
}

// ScaleVMResources scales both CPU and memory in one operation
func ScaleVMResources(ctx context.Context, socketPath string, vcpus int, memoryMB int64) error {
	client := NewCLHClient(socketPath)

	resize := &VmResize{
		DesiredVcpus: &vcpus,
		DesiredRam:   &memoryMB,
	}

	return client.VmResize(ctx, resize)
}

// HotplugDisk is a convenience function for adding a disk at runtime
func HotplugDisk(ctx context.Context, socketPath, diskPath, diskID string, readonly bool) (*PciDeviceInfo, error) {
	client := NewCLHClient(socketPath)

	disk := &DiskConfig{
		Path:     diskPath,
		Readonly: readonly,
		ID:       diskID,
	}

	return client.VmAddDisk(ctx, disk)
}

// HotplugNetwork is a convenience function for adding a network interface at runtime
func HotplugNetwork(ctx context.Context, socketPath, tap, mac, id string) (*PciDeviceInfo, error) {
	client := NewCLHClient(socketPath)

	net := &NetConfig{
		Tap: tap,
		Mac: mac,
		ID:  id,
	}

	return client.VmAddNet(ctx, net)
}

// GetVMStatus retrieves current VM state and basic info
func GetVMStatus(ctx context.Context, socketPath string) (state string, info *VmInfo, err error) {
	client := NewCLHClient(socketPath)

	info, err = client.VmInfo(ctx)
	if err != nil {
		return "", nil, err
	}

	return info.State, info, nil
}

// IsVMRunning checks if a VM is in running state
func IsVMRunning(ctx context.Context, socketPath string) bool {
	client := NewCLHClient(socketPath)
	state, err := client.GetState(ctx)
	if err != nil {
		return false
	}
	return state == VmStateRunning || state == VmStateRunningVirtualized
}

// IsVMPaused checks if a VM is in paused state
func IsVMPaused(ctx context.Context, socketPath string) bool {
	client := NewCLHClient(socketPath)
	state, err := client.GetState(ctx)
	if err != nil {
		return false
	}
	return state == VmStatePaused
}

// CheckAPIHealth performs a health check on the Cloud Hypervisor API
func CheckAPIHealth(socketPath string) error {
	client := NewCLHClientWithTimeout(socketPath, 2*time.Second)
	return client.Ping()
}
