package compute

import (
	"context"
	"encoding/json"
)

// Type identifies a registered hypervisor backend.
type Type string

const (
	TypeCloudHypervisor Type = "cloud_hypervisor"
	TypeFirecracker     Type = "firecracker"
)

// DiskFormat describes the root volume backing format.
type DiskFormat string

const (
	FormatRaw    DiskFormat = "raw"
	FormatQcow2  DiskFormat = "qcow2"
	FormatQcow2Flat DiskFormat = "qcow2-flat"
)

// Volume is a boot disk reference passed by value.
type Volume struct {
	Path   string
	Format DiskFormat
}

// VMConfig is the serializable boot/snapshot payload for hypervisor plugins.
type VMConfig struct {
	ID             string
	VCPU           int
	MemMB          int
	KernelPath     string
	InitrdPath     string
	KernelCmdline  string
	ImageType      string
	RootVolume     Volume
	EnableSecurity bool
	InstanceDir    string
	NetNSName      string
	TapName        string
	MacAddress     string
	IPAddress      string
	OverlayPath    string
	DebugConsole   bool
	SnapshotDir    string
}

// VMState is the normalized hypervisor state.
type VMState string

const (
	VMStateNotStarted VMState = "not_started"
	VMStateRunning    VMState = "running"
	VMStatePaused     VMState = "paused"
	VMStateStopped    VMState = "stopped"
	VMStateDead       VMState = "dead"
)

// Hypervisor is the plugin contract between core and VMM backends.
type Hypervisor interface {
	Name() string

	StartVM(ctx context.Context, cfg VMConfig) error
	StopVM(ctx context.Context, id string) error
	StartGuest(ctx context.Context, id string) error
	PauseVM(ctx context.Context, id string) error
	ResumeVM(ctx context.Context, id string) error
	DeleteVM(ctx context.Context, id string) error
	Snapshot(ctx context.Context, id string, snapshotDir string) error
	Restore(ctx context.Context, cfg VMConfig) error

	GetState(ctx context.Context, id string) (VMState, error)
	Info(ctx context.Context, id string) (json.RawMessage, error)
	IsAvailable(id string) bool
	Counters(ctx context.Context, id string) (json.RawMessage, error)
}
