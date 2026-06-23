package runtime

import (
	"context"
	"fmt"

	"voidrun/config"
	"voidrun/model"
)

// Known hypervisor type identifiers.
const (
	DriverCloudHypervisor = "cloud-hypervisor"
	DriverFirecracker     = "firecracker"
)

// VMDriver abstracts hypervisor-specific VM lifecycle operations so that the
// service layer can work with both Cloud Hypervisor and Firecracker without
// being aware of the underlying implementation.
//
// All implementations must be safe for concurrent use.
type VMDriver interface {
	// Name returns the driver identifier (e.g. "cloud-hypervisor" or "firecracker").
	Name() string

	// CreateCLI spawns the hypervisor process with the full VM configuration
	// embedded in CLI flags (CLH) or configured via API (Firecracker), then
	// boots the VM. This is the primary path for initial sandbox creation.
	CreateCLI(cfg config.Config, spec model.SandboxSpec, overlayPath string) error

	// Create spawns an empty hypervisor, injects configuration via the
	// hypervisor's REST API, and boots the VM. Used for cold restart when the
	// hypervisor process is no longer running. For Firecracker this is
	// equivalent to CreateCLI since FC is always API-configured.
	Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error

	// Start boots a VM that is in a halted/shutdown state while the hypervisor
	// process is still alive (warm restart, supported by CLH only).
	// Firecracker does not keep the process alive after a halt, so calling
	// Start on a Firecracker driver always returns an error.
	Start(id string) error

	// Stop gracefully shuts down the guest OS.
	// For CLH: sends vm.shutdown, keeping the hypervisor process alive.
	// For Firecracker: sends SendCtrlAltDel ACPI signal and kills the process.
	Stop(id string) error

	// Pause suspends VM execution, preserving all CPU/memory state in place.
	Pause(id string) error

	// Resume resumes a previously paused VM.
	Resume(id string) error

	// Delete shuts down and kills the hypervisor process, then tears down the
	// sandbox network namespace.
	Delete(id, tapName, nsName string) error

	// IsSocketAvailable returns true if the hypervisor control socket is
	// reachable, indicating the hypervisor process is alive.
	IsSocketAvailable(id string) bool

	// GetStateWithContext queries the hypervisor for the current VM state and
	// returns it as one of the normalised application states:
	// "running", "paused", "stopped", or "killed".
	GetStateWithContext(ctx context.Context, id string) (string, error)

	// Info returns a raw JSON string with detailed VM information from the
	// hypervisor API. Intended for debugging and the /info endpoint.
	Info(id string) (string, error)

	// SocketPath returns the filesystem path to the hypervisor control socket
	// for the given sandbox. Used by the metrics manager.
	SocketPath(id string) string

	// OverlayPath returns the filesystem path to the disk overlay image that
	// the hypervisor expects. CLH uses qcow2; Firecracker uses raw.
	OverlayPath(id string) string
}

// NewVMDriver creates the correct VMDriver implementation based on the
// HypervisorType field in cfg.
func NewVMDriver(cfg *config.Config) (VMDriver, error) {
	switch cfg.HypervisorType {
	case DriverCloudHypervisor, "":
		return &CLHDriver{}, nil
	case DriverFirecracker:
		return &FCDriver{}, nil
	default:
		return nil, fmt.Errorf(
			"unknown HYPERVISOR_TYPE %q (valid values: %s, %s)",
			cfg.HypervisorType, DriverCloudHypervisor, DriverFirecracker,
		)
	}
}
