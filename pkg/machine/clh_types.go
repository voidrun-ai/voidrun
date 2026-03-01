package machine

// CLH API Request/Response Types
// Based on Cloud Hypervisor API v0.49+
// Reference: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/api.md

// VmConfig is the complete VM configuration for vm.create
// Note: Basic types (CpusConfig, MemoryConfig, etc.) are defined in clh_config.go
type VmConfig struct {
	Cpus        *CpusConfig        `json:"cpus,omitempty"`
	Memory      *MemoryConfig      `json:"memory,omitempty"`
	Payload     *PayloadConfig     `json:"payload,omitempty"`
	Disks       []DiskConfig       `json:"disks,omitempty"`
	Net         []NetConfig        `json:"net,omitempty"`
	Rng         *RngConfig         `json:"rng,omitempty"`
	Balloon     *BalloonConfig     `json:"balloon,omitempty"`
	Fs          []FsConfig         `json:"fs,omitempty"`
	Pmem        []PmemConfig       `json:"pmem,omitempty"`
	Serial      *ConsoleConfig     `json:"serial,omitempty"`
	Console     *ConsoleConfig     `json:"console,omitempty"`
	Devices     []DeviceConfig     `json:"devices,omitempty"`
	UserDevices []UserDeviceConfig `json:"user_devices,omitempty"`
	Vdpa        []VdpaConfig       `json:"vdpa,omitempty"`
	Vsock       *VsockConfig       `json:"vsock,omitempty"`
	Tpm         *TpmConfig         `json:"tpm,omitempty"`
	Iommu       bool               `json:"iommu,omitempty"`
	Platform    *PlatformConfig    `json:"platform,omitempty"`
}

// Additional device configuration types
type BalloonConfig struct {
	Size           int64 `json:"size"`
	DeflateOnOOM   bool  `json:"deflate_on_oom,omitempty"`
	FreePageReport bool  `json:"free_page_reporting,omitempty"`
}

type FsConfig struct {
	Tag        string `json:"tag"`
	Socket     string `json:"socket"`
	NumQueues  int    `json:"num_queues,omitempty"`
	QueueSize  int    `json:"queue_size,omitempty"`
	ID         string `json:"id,omitempty"`
	PciSegment int    `json:"pci_segment,omitempty"`
}

type PmemConfig struct {
	File          string `json:"file"`
	Size          int64  `json:"size,omitempty"`
	Iommu         bool   `json:"iommu,omitempty"`
	DiscardWrites bool   `json:"discard_writes,omitempty"`
	ID            string `json:"id,omitempty"`
	PciSegment    int    `json:"pci_segment,omitempty"`
}

type DeviceConfig struct {
	Path       string `json:"path"`
	Iommu      bool   `json:"iommu,omitempty"`
	ID         string `json:"id,omitempty"`
	PciSegment int    `json:"pci_segment,omitempty"`
}

type UserDeviceConfig struct {
	Socket     string `json:"socket"`
	ID         string `json:"id,omitempty"`
	PciSegment int    `json:"pci_segment,omitempty"`
}

type VdpaConfig struct {
	Path       string `json:"path"`
	NumQueues  int    `json:"num_queues,omitempty"`
	ID         string `json:"id,omitempty"`
	PciSegment int    `json:"pci_segment,omitempty"`
}

type TpmConfig struct {
	Socket string `json:"socket"`
}

type PlatformConfig struct {
	NumPciSegments int      `json:"num_pci_segments,omitempty"`
	IommuSegments  []int    `json:"iommu_segments,omitempty"`
	SerialNumber   string   `json:"serial_number,omitempty"`
	UUID           string   `json:"uuid,omitempty"`
	OEMStrings     []string `json:"oem_strings,omitempty"`
}

// VmResize is used for resizing CPU or memory
type VmResize struct {
	DesiredVcpus   *int   `json:"desired_vcpus,omitempty"`
	DesiredRam     *int64 `json:"desired_ram,omitempty"`
	DesiredBalloon *int64 `json:"desired_balloon,omitempty"`
}

// VmResizeDisk is used for resizing disks
type VmResizeDisk struct {
	DiskID  string `json:"disk_id"`
	NewSize int64  `json:"new_size"`
}

// VmResizeZone is used for resizing memory zones
type VmResizeZone struct {
	ID         string `json:"id"`
	DesiredRam int64  `json:"desired_ram"`
}

// VmAddDevice is used for hotplugging VFIO devices
type VmAddDevice struct {
	DeviceConfig
}

// VmAddDisk is used for hotplugging disks
type VmAddDisk struct {
	DiskConfig
}

// VmAddFs is used for hotplugging fs devices
type VmAddFs struct {
	FsConfig
}

// VmAddPmem is used for hotplugging pmem devices
type VmAddPmem struct {
	PmemConfig
}

// VmAddNet is used for hotplugging network devices
type VmAddNet struct {
	NetConfig
}

// VmAddUserDevice is used for hotplugging userspace devices
type VmAddUserDevice struct {
	UserDeviceConfig
}

// VmAddVdpa is used for hotplugging vdpa devices
type VmAddVdpa struct {
	VdpaConfig
}

// VmAddVsock is used for hotplugging vsock devices
type VmAddVsock struct {
	VsockConfig
}

// VmRemoveDevice is used for removing hotplugged devices
type VmRemoveDevice struct {
	ID string `json:"id"`
}

// VmSnapshotConfig is used for creating snapshots
type VmSnapshotConfig struct {
	DestinationURL string `json:"destination_url"`
}

// VmCoredumpData is used for creating coredumps (x86_64 only)
type VmCoredumpData struct {
	DestinationURL string `json:"destination_url"`
}

// RestoreConfig is used for restoring from snapshots
type RestoreConfig struct {
	SourceURL string      `json:"source_url"`
	Prefault  bool        `json:"prefault,omitempty"`
	Net       []NetConfig `json:"net_fds,omitempty"`
}

// ReceiveMigrationData is used for receiving migrations
type ReceiveMigrationData struct {
	ReceiverURL string `json:"receiver_url"`
}

// SendMigrationData is used for sending migrations
type SendMigrationData struct {
	DestinationURL string `json:"destination_url"`
	Local          bool   `json:"local,omitempty"`
}

// VmmPingResponse is returned by vmm.ping
type VmmPingResponse struct {
	BuildVersion string `json:"build_version"`
	Version      string `json:"version"`
	PID          int    `json:"pid,omitempty"`
}

// VmInfo is returned by vm.info
type VmInfo struct {
	Config           VmConfig               `json:"config"`
	State            string                 `json:"state"`
	MemoryActualSize int64                  `json:"memory_actual_size,omitempty"`
	DeviceTree       map[string]interface{} `json:"device_tree,omitempty"`
}

// PciDeviceInfo is returned by device hotplug operations
type PciDeviceInfo struct {
	ID  string `json:"id"`
	BDF string `json:"bdf"`
}

// VmCounters is returned by vm.counters
type VmCounters struct {
	// Structure depends on enabled features and devices
	// Using generic map for flexibility
	Counters map[string]interface{} `json:",inline"`
}

const (
	VmStateCreated            = "Created"
	VmStateRunning            = "Running"
	VmStateShutdown           = "Shutdown"
	VmStatePaused             = "Paused"
	VmStateRunningVirtualized = "RunningVirtualized" // Rare state
	VmStateLoaded             = "Loaded"             // After restore but before boot
)
