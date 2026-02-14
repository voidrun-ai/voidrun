package machine

// CLHConfig matches the Cloud Hypervisor v0.49+ API Schema
type CLHConfig struct {
	Payload PayloadConfig `json:"payload"`
	Cpus    CpusConfig    `json:"cpus"`
	Memory  MemoryConfig  `json:"memory"`
	Disks   []DiskConfig  `json:"disks"`
	Net     []NetConfig   `json:"net"`
	Rng     RngConfig     `json:"rng"`
	Serial  ConsoleConfig `json:"serial"`
	Console ConsoleConfig `json:"console"`
	Vsock   *VsockConfig  `json:"vsock,omitempty"`
}

type PayloadConfig struct {
	Kernel    string `json:"kernel"`
	CmdLine   string `json:"cmdline"`
	Initramfs string `json:"initramfs,omitempty"`
}

type CpusConfig struct {
	BootVcpus int `json:"boot_vcpus"`
	MaxVcpus  int `json:"max_vcpus"`
}

type MemoryConfig struct {
	Size      int64 `json:"size"`
	Shared    bool  `json:"shared"`
	Mergeable bool  `json:"mergeable"`
	Prefault  bool  `json:"prefault"`
}

type DiskConfig struct {
	Path string `json:"path"`
}

type NetConfig struct {
	Tap string `json:"tap"`
	Mac string `json:"mac"`
	IP  string `json:"ip,omitempty"` // Optional, but accepted by API
}

type RngConfig struct {
	Src string `json:"src"`
}

type ConsoleConfig struct {
	Mode string `json:"mode"` // "Null", "Tty", "File"
}

type VsockConfig struct {
	Cid    uint64 `json:"cid"`
	Socket string `json:"socket"`
}
