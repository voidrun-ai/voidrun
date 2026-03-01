package machine

// CLHConfig matches the Cloud Hypervisor v0.49+ API Schema
// Alias for VmConfig for backward compatibility
type CLHConfig = VmConfig

// PayloadConfig defines kernel and boot configuration
type PayloadConfig struct {
	Kernel    string `json:"kernel,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
	Initramfs string `json:"initramfs,omitempty"`
	Firmware  string `json:"firmware,omitempty"`
}

// CpusConfig defines CPU configuration
type CpusConfig struct {
	BootVcpus   int           `json:"boot_vcpus"`
	MaxVcpus    int           `json:"max_vcpus"`
	Topology    *CpuTopology  `json:"topology,omitempty"`
	KvmHyperv   interface{}   `json:"kvm_hyperv,omitempty"` // Can be bool or object depending on CLH version
	MaxPhysBits int           `json:"max_phys_bits,omitempty"`
	Affinity    []CpuAffinity `json:"affinity,omitempty"`
	Features    *CpuFeatures  `json:"features,omitempty"`
}

type CpuTopology struct {
	ThreadsPerCore int `json:"threads_per_core,omitempty"`
	CoresPerDie    int `json:"cores_per_die,omitempty"`
	DiesPerPackage int `json:"dies_per_package,omitempty"`
	Packages       int `json:"packages,omitempty"`
}

type KvmConfig struct {
	NoKvmHyperv bool `json:"no_kvm_hyperv,omitempty"`
}

type CpuAffinity struct {
	Vcpu     int   `json:"vcpu"`
	HostCpus []int `json:"host_cpus"`
}

type CpuFeatures struct {
	Amx bool `json:"amx,omitempty"`
}

// MemoryConfig defines memory configuration
type MemoryConfig struct {
	Size           int64              `json:"size"`
	Mergeable      bool               `json:"mergeable,omitempty"`
	Shared         bool               `json:"shared,omitempty"`
	Hugepages      bool               `json:"hugepages,omitempty"`
	HugepageSize   int64              `json:"hugepage_size,omitempty"`
	Hotplug        interface{}        `json:"hotplug_method,omitempty"` // Can be bool or string depending on CLH version
	HotplugSize    int64              `json:"hotplug_size,omitempty"`
	HotpluggedSize int64              `json:"hotplugged_size,omitempty"`
	Prefault       bool               `json:"prefault,omitempty"`
	Zones          []MemoryZoneConfig `json:"zones,omitempty"`
}

type MemoryZoneConfig struct {
	ID             string `json:"id"`
	Size           int64  `json:"size"`
	File           string `json:"file,omitempty"`
	Shared         bool   `json:"shared,omitempty"`
	Hugepages      bool   `json:"hugepages,omitempty"`
	HugepageSize   int64  `json:"hugepage_size,omitempty"`
	HostNumaNode   int    `json:"host_numa_node,omitempty"`
	Hotplug        bool   `json:"hotplug,omitempty"`
	HotpluggedSize int64  `json:"hotplugged_size,omitempty"`
	Prefault       bool   `json:"prefault,omitempty"`
}

// DiskConfig defines disk configuration
type DiskConfig struct {
	Path           string `json:"path"`
	Readonly       bool   `json:"readonly,omitempty"`
	Direct         bool   `json:"direct,omitempty"`
	Iommu          bool   `json:"iommu,omitempty"`
	NumQueues      int    `json:"num_queues,omitempty"`
	QueueSize      int    `json:"queue_size,omitempty"`
	VhostUser      bool   `json:"vhost_user,omitempty"`
	VhostSocket    string `json:"vhost_socket,omitempty"`
	RateLimitGroup string `json:"rate_limit_group,omitempty"`
	ID             string `json:"id,omitempty"`
	DisableIO      bool   `json:"disable_io_uring,omitempty"`
	PciSegment     int    `json:"pci_segment,omitempty"`
}

// NetConfig defines network configuration
type NetConfig struct {
	Tap            string `json:"tap,omitempty"`
	IP             string `json:"ip,omitempty"`
	Mask           string `json:"mask,omitempty"`
	Mac            string `json:"mac,omitempty"`
	HostMac        string `json:"host_mac,omitempty"`
	MTU            int    `json:"mtu,omitempty"`
	Iommu          bool   `json:"iommu,omitempty"`
	NumQueues      int    `json:"num_queues,omitempty"`
	QueueSize      int    `json:"queue_size,omitempty"`
	VhostUser      bool   `json:"vhost_user,omitempty"`
	VhostSocket    string `json:"vhost_socket,omitempty"`
	VhostMode      string `json:"vhost_mode,omitempty"`
	ID             string `json:"id,omitempty"`
	FDs            []int  `json:"fds,omitempty"`
	RateLimitGroup string `json:"rate_limit_group,omitempty"`
	PciSegment     int    `json:"pci_segment,omitempty"`
}

// RngConfig defines RNG configuration
type RngConfig struct {
	Src   string `json:"src"`
	Iommu bool   `json:"iommu,omitempty"`
}

// ConsoleConfig defines console configuration
type ConsoleConfig struct {
	Mode  string `json:"mode"` // "Off", "Tty", "File", "Null"
	File  string `json:"file,omitempty"`
	Iommu bool   `json:"iommu,omitempty"`
}

// VsockConfig defines vsock configuration
type VsockConfig struct {
	Cid        uint64 `json:"cid"`
	Socket     string `json:"socket"`
	Iommu      bool   `json:"iommu,omitempty"`
	ID         string `json:"id,omitempty"`
	PciSegment int    `json:"pci_segment,omitempty"`
}
