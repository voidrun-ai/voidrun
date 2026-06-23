package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"voidrun/config"
	"voidrun/model"
)

// CLHDriver implements VMDriver for Cloud Hypervisor.
// It delegates to the existing lifecycle functions so that behaviour is
// unchanged from the pre-driver-interface code.
type CLHDriver struct{}

// Name returns the driver identifier.
func (d *CLHDriver) Name() string { return DriverCloudHypervisor }

// CreateCLI spawns CLH with full CLI config and boots the VM.
func (d *CLHDriver) CreateCLI(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	return CreateCLI(cfg, spec, overlayPath)
}

// Create spawns an empty CLH, injects config via API, then boots the VM.
func (d *CLHDriver) Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	return Create(cfg, spec, overlayPath)
}

// Start boots a CLH VM that is in shutdown state (warm restart).
func (d *CLHDriver) Start(id string) error {
	return Start(id)
}

// Stop gracefully shuts down the guest, keeping the CLH process alive.
func (d *CLHDriver) Stop(id string) error {
	return Stop(id)
}

// Pause suspends VM execution via CLH API.
func (d *CLHDriver) Pause(id string) error {
	return Pause(id)
}

// Resume resumes a paused VM via CLH API.
func (d *CLHDriver) Resume(id string) error {
	return Resume(id)
}

// Delete shuts down and kills the CLH process, then destroys the netns.
func (d *CLHDriver) Delete(id, tapName, nsName string) error {
	return Delete(id, tapName, nsName)
}

// IsSocketAvailable returns true if the CLH control socket is reachable.
func (d *CLHDriver) IsSocketAvailable(id string) bool {
	client := NewCLHClient(GetSocketPath(id))
	return client.IsSocketAvailable()
}

// GetStateWithContext queries CLH for the current VM state and maps it to an
// application-level state string.
func (d *CLHDriver) GetStateWithContext(ctx context.Context, id string) (string, error) {
	client := NewAPIClientForSandbox(id)
	clhState, err := client.GetStateWithContext(ctx)
	if err != nil {
		return "killed", fmt.Errorf("CLH state query failed: %w", err)
	}

	switch strings.ToLower(clhState) {
	case "running", "runningvirtualized":
		return "running", nil
	case "paused":
		return "paused", nil
	case "loaded":
		// CLH process alive but guest not yet booted — treated as stopped.
		return "stopped", nil
	default:
		return "stopped", nil
	}
}

// Info returns raw JSON VM info from CLH.
func (d *CLHDriver) Info(id string) (string, error) {
	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return "", fmt.Errorf("sandbox not running (socket missing)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := client.VmInfo(ctx)
	if err != nil {
		return "", err
	}

	jsonBytes, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("failed to marshal info: %w", err)
	}
	return string(jsonBytes), nil
}

// SocketPath returns the CLH API socket path for the given sandbox.
func (d *CLHDriver) SocketPath(id string) string { return GetSocketPath(id) }

// OverlayPath returns the qcow2 overlay path used by CLH.
func (d *CLHDriver) OverlayPath(id string) string { return GetOverlayPath(id) }
