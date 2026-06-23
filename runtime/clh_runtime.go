package runtime

import (
	"context"
	"strings"
	"time"

	"voidrun/config"
	"voidrun/model"
)

// CLHRuntime implements HypervisorRuntime for Cloud Hypervisor.
// It wraps the existing free-function implementation so that the rest of the
// codebase can interact with CLH through the common interface.
type CLHRuntime struct{}

// NewCLHRuntime returns a CLHRuntime ready for use.
func NewCLHRuntime() *CLHRuntime {
	return &CLHRuntime{}
}

// Type returns the hypervisor identifier.
func (r *CLHRuntime) Type() HypervisorType {
	return HypervisorCloudHypervisor
}

// SupportsPause returns true; CLH natively supports pause/resume.
func (r *CLHRuntime) SupportsPause() bool {
	return true
}

// IsSocketAvailable reports whether the CLH API socket file is present.
func (r *CLHRuntime) IsSocketAvailable(id string) bool {
	return NewCLHClient(GetSocketPath(id)).IsSocketAvailable()
}

// Create spawns a Cloud Hypervisor process with the full configuration passed
// on the command line (CLI mode) and boots the VM.  Used for both initial
// sandbox creation and cold restarts after the CLH process has died.
func (r *CLHRuntime) Create(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	return CreateCLI(cfg, spec, overlayPath)
}

// WarmStart boots a VM that is in shutdown state using the still-running CLH
// process.  Only valid when IsSocketAvailable returns true.
func (r *CLHRuntime) WarmStart(id string) error {
	return Start(id)
}

// Stop sends vm.shutdown to the running CLH process.  The CLH process and
// network namespace are preserved so the sandbox can be restarted cheaply.
func (r *CLHRuntime) Stop(id string) error {
	return Stop(id)
}

// Delete sends vm.delete to the CLH API, kills the CLH process, and destroys
// the network namespace.  Disk files are left for event-monitor finalisation.
func (r *CLHRuntime) Delete(id, tapName, nsName string) error {
	return Delete(id, tapName, nsName)
}

// Pause suspends VM execution via the CLH vm.pause API.
func (r *CLHRuntime) Pause(id string) error {
	return Pause(id)
}

// Resume resumes a paused VM via the CLH vm.resume API.
func (r *CLHRuntime) Resume(id string) error {
	return Resume(id)
}

// GetState queries the CLH API and returns a normalised state string.
func (r *CLHRuntime) GetState(id string) (string, error) {
	client := NewCLHClient(GetSocketPath(id))
	if !client.IsSocketAvailable() {
		return "stopped", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	raw, err := client.GetState(ctx)
	if err != nil {
		return "unknown", err
	}

	return normaliseCLHState(raw), nil
}

// Info returns the raw CLH vm.info response as JSON.
func (r *CLHRuntime) Info(id string) (string, error) {
	return Info(id)
}

// normaliseCLHState maps CLH-specific state strings to the application-level
// status vocabulary ("running", "paused", "stopped", "unknown").
func normaliseCLHState(raw string) string {
	switch strings.ToLower(raw) {
	case "running", "runningvirtualized":
		return "running"
	case "paused":
		return "paused"
	case "shutdown", "loaded", "created":
		return "stopped"
	default:
		return "unknown"
	}
}
