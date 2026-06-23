package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"voidrun/config"
	"voidrun/model"
)

// ErrUnsupported is returned by Hypervisor implementations when an operation
// is not supported by the current backend (e.g. coredump on Firecracker).
var ErrUnsupported = errors.New("operation not supported by this hypervisor")

// HypervisorType is the canonical identifier for a backend.
type HypervisorType string

const (
	HypervisorCloudHypervisor HypervisorType = "cloud-hypervisor"
	HypervisorFirecracker     HypervisorType = "firecracker"
)

// NormalizedState is the hypervisor-agnostic VM state surfaced to the rest of
// the application. Each backend maps its native states into this set.
type NormalizedState string

const (
	StateUnknown NormalizedState = "unknown"
	StateCreated NormalizedState = "created"
	StateRunning NormalizedState = "running"
	StatePaused  NormalizedState = "paused"
	StateStopped NormalizedState = "stopped"
	StateKilled  NormalizedState = "killed"
)

// Capabilities describes which optional hypervisor features a backend exposes.
type Capabilities struct {
	SupportsHotplugDisk    bool
	SupportsHotplugNetwork bool
	SupportsSnapshot       bool
	SupportsCoreDump       bool
	// SupportsQcow2Disks indicates the backend can boot from a qcow2 overlay
	// (with or without backing file). Firecracker accepts raw images only.
	SupportsQcow2Disks bool
	// SupportsCtrlAltDel reports whether the backend has an in-band graceful
	// shutdown signal. CLH (ACPI) supports it on every arch; Firecracker is
	// x86_64 only.
	SupportsCtrlAltDel bool
}

// CountersSnapshot is the normalized form of per-sandbox resource counters
// scraped by the metrics manager.
type CountersSnapshot struct {
	// CPUUsage may be reported either as a fraction (0..1) or percentage (0..100).
	// The caller normalises to 0..100 for Prometheus.
	CPUUsage         float64
	MemoryUsedBytes  uint64
	Disks            map[string]DiskCountersSnapshot
	Nets             map[string]NetCountersSnapshot
}

type DiskCountersSnapshot struct {
	ReadBytes       uint64
	WriteBytes      uint64
	ReadOps         uint64
	WriteOps        uint64
	ReadLatencyMin  float64
	ReadLatencyMax  float64
	ReadLatencyAvg  float64
	WriteLatencyMin float64
	WriteLatencyMax float64
	WriteLatencyAvg float64
}

type NetCountersSnapshot struct {
	RxBytes  uint64
	TxBytes  uint64
	RxFrames uint64
	TxFrames uint64
}

// EventBatch is a chunk of native event payloads pulled from a hypervisor
// event source. The runtime.EventMonitor decodes these into model.SandboxEvent.
type EventBatch struct {
	// Source is the canonical source label written into model.SandboxEvent.Source
	// (e.g. "clh", "fc").
	Source string
	// Events is a list of decoded events (one entry per native event record).
	Events []EventRecord
}

// EventRecord is one native event in a backend-agnostic shape.
type EventRecord struct {
	Event      string
	UptimeNs   int64
	Properties map[string]any
}

// EventSource is a tailing source for a single sandbox's event stream. The
// EventMonitor polls it periodically and persists records via the EventSink.
type EventSource interface {
	// Source returns the canonical label written to model.SandboxEvent.Source.
	Source() string
	// OffsetPath is the on-disk byte-offset tracker used by EventMonitor.
	OffsetPath() string
	// Poll reads new events starting at the given byte offset and returns the
	// records plus the new offset. If the underlying file does not yet exist
	// it returns (nil, offset, nil) so callers can keep polling.
	Poll(ctx context.Context, offset int64) ([]EventRecord, int64, error)
}

// Hypervisor is the backend-agnostic contract for managing a sandbox VM.
type Hypervisor interface {
	Name() string
	Capabilities() Capabilities

	// Boot spawns the hypervisor process and brings the VM up. The provided
	// spec must already have its TAP/NetNS/MAC fields populated (network setup
	// is performed by the caller via ConfigureNetwork).
	Boot(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath string) error

	// Start boots a VM whose hypervisor process is already running but whose
	// guest is in a stopped/created state (warm restart). Returns ErrUnsupported
	// when the backend does not distinguish warm/cold restarts.
	Start(ctx context.Context, id string) error

	// Stop performs an in-guest graceful shutdown but leaves the hypervisor
	// process running so a subsequent Start can warm-boot.
	Stop(ctx context.Context, id string) error

	// Pause suspends VM execution (state preserved in RAM).
	Pause(ctx context.Context, id string) error

	// Resume resumes a paused VM.
	Resume(ctx context.Context, id string) error

	// Delete tears down the hypervisor process and any host resources
	// (TAP, network namespace). Instance files on disk are removed by the
	// caller via runtime.Cleanup after any final event sync.
	Delete(ctx context.Context, id, tapName, nsName string) error

	// State reports the normalised VM state. Returns StateStopped when the
	// hypervisor process is gone.
	State(ctx context.Context, id string) (NormalizedState, error)

	// Info returns a raw JSON snapshot of the backend-native VM info, used
	// for debugging endpoints.
	Info(ctx context.Context, id string) (string, error)

	// IsSocketAvailable is a fast (non-API) check for whether the hypervisor
	// management socket exists on disk.
	IsSocketAvailable(id string) bool

	// EventSource returns the per-sandbox event source the EventMonitor should
	// tail. Returns nil if the backend does not produce events for this sandbox.
	EventSource(id string) EventSource

	// Counters returns a backend-agnostic snapshot of CPU/mem/disk/net counters.
	Counters(ctx context.Context, id string) (*CountersSnapshot, error)
}

// HypervisorResolver picks the backend to use for a given sandbox. The default
// is returned when sandbox.Hypervisor is empty (i.e. legacy rows).
type HypervisorResolver interface {
	Default() Hypervisor
	For(sandbox *model.Sandbox) Hypervisor
}

// staticResolver is the resolver used while only one backend is registered.
type staticResolver struct {
	def        Hypervisor
	registered map[HypervisorType]Hypervisor
}

func (r *staticResolver) Default() Hypervisor { return r.def }

func (r *staticResolver) For(sandbox *model.Sandbox) Hypervisor {
	if sandbox == nil || strings.TrimSpace(sandbox.Hypervisor) == "" {
		return r.def
	}
	if hv, ok := r.registered[HypervisorType(sandbox.Hypervisor)]; ok {
		return hv
	}
	return r.def
}

// NewResolver constructs a HypervisorResolver from the application config.
// At present only the cloud-hypervisor backend is registered.
func NewResolver(cfg *config.Config) (HypervisorResolver, error) {
	registered := map[HypervisorType]Hypervisor{}

	clh := NewCLHBackend(cfg)
	registered[HypervisorCloudHypervisor] = clh

	t := HypervisorType(strings.TrimSpace(string(cfg.Hypervisor.Type)))
	if t == "" {
		t = HypervisorCloudHypervisor
	}
	def, ok := registered[t]
	if !ok {
		return nil, fmt.Errorf("unknown hypervisor type %q (supported: %s)",
			t, supportedHypervisorList(registered))
	}
	return &staticResolver{def: def, registered: registered}, nil
}

func supportedHypervisorList(m map[HypervisorType]Hypervisor) string {
	parts := make([]string, 0, len(m))
	for k := range m {
		parts = append(parts, string(k))
	}
	return strings.Join(parts, ", ")
}
