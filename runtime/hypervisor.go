package runtime

import (
	"errors"
	"voidrun/config"
	"voidrun/model"
)

// HypervisorType identifies which hypervisor backs a sandbox VM.
type HypervisorType string

const (
	HypervisorCloudHypervisor HypervisorType = "cloud-hypervisor"
	HypervisorFirecracker      HypervisorType = "firecracker"
)

// ErrWarmStartNotSupported is returned by hypervisors that do not keep a
// persistent management process while the VM is stopped (e.g. Firecracker).
var ErrWarmStartNotSupported = errors.New("hypervisor does not support warm start; use cold boot via Create")

// ErrPauseNotSupported is returned when a hypervisor build does not expose
// pause/resume (should not occur for either CLH or modern Firecracker, but
// kept as a sentinel so callers can handle it gracefully).
var ErrPauseNotSupported = errors.New("hypervisor does not support pause/resume")

// HypervisorRuntime is the single interface through which the service layer
// manages VM lifecycle, regardless of which hypervisor is in use.
//
// Implementations must be safe for concurrent use from multiple goroutines.
// Path helpers (GetSocketPath, GetVsockPath, …) remain package-level so they
// are accessible from non-hypervisor code (metrics, agent dial, etc.) without
// needing a runtime instance.
type HypervisorRuntime interface {
	// Type returns the hypervisor identifier for this implementation.
	Type() HypervisorType

	// Create spawns the hypervisor process, configures the VM with the given
	// spec and overlay disk, and boots the guest.  It is called for both
	// initial sandbox creation and cold restarts after the process has died.
	Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error

	// WarmStart boots a VM that is in a stopped/shutdown state while the
	// hypervisor management process is still running.
	//
	// Cloud Hypervisor: sends vm.boot to the living CLH process.
	// Firecracker: returns ErrWarmStartNotSupported because Firecracker has
	// no persistent process after a stop — callers should fall back to Create.
	WarmStart(id string) error

	// Stop gracefully shuts down the VM guest while preserving the hypervisor
	// process and network namespace so the sandbox can be restarted cheaply.
	//
	// Firecracker note: because Firecracker has no "stopped-but-alive" state,
	// Stop kills the process entirely.  A subsequent WarmStart will therefore
	// fail; callers must use Create for the next boot.
	Stop(id string) error

	// Delete terminates the hypervisor process and destroys the network
	// namespace.  Disk files are intentionally left on disk so the event
	// monitor can finish syncing before Cleanup is called.
	Delete(id, tapName, nsName string) error

	// Pause suspends VM execution with its in-memory state intact.
	// Returns ErrPauseNotSupported if the underlying hypervisor build does not
	// expose this capability.
	Pause(id string) error

	// Resume unsuspends a previously paused VM.
	Resume(id string) error

	// GetState returns the current VM state normalised to one of the
	// application-level strings: "running", "paused", "stopped", "unknown".
	GetState(id string) (string, error)

	// Info returns raw hypervisor-specific VM information serialised as JSON.
	Info(id string) (string, error)

	// IsSocketAvailable reports whether the hypervisor's control socket
	// (used as the process-alive liveness signal) is present on disk.
	IsSocketAvailable(id string) bool

	// SupportsPause reports whether this implementation supports Pause/Resume.
	SupportsPause() bool
}
