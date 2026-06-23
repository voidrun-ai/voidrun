package runtime

// ---------------------------------------------------------------------------
// Firecracker REST API types
//
// Reference: https://github.com/firecracker-microvm/firecracker/blob/main/src/api_server/swagger/firecracker.yaml
// ---------------------------------------------------------------------------

// FCInstanceInfo is returned by GET / (describe instance).
type FCInstanceInfo struct {
	ID             string `json:"id"`
	State          string `json:"state"` // "Not started" | "Running" | "Paused"
	VMVersion      string `json:"vmm_version,omitempty"`
	AppName        string `json:"app_name,omitempty"`
}

// FCMachineConfig configures vCPUs, memory and optional SMT/huge-pages.
type FCMachineConfig struct {
	VcpuCount       int  `json:"vcpu_count"`
	MemSizeMib      int  `json:"mem_size_mib"`
	SMT             bool `json:"smt,omitempty"`
	TrackDirtyPages bool `json:"track_dirty_pages,omitempty"`
}

// FCBootSource specifies the kernel image and boot arguments.
type FCBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// FCDrive configures a block device (rootfs or additional data disk).
// Firecracker only supports raw disk images.
type FCDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	RateLimiter  *FCRateLimiter `json:"rate_limiter,omitempty"`
}

// FCRateLimiter provides optional IOPS/bandwidth throttling for drives.
type FCRateLimiter struct {
	Bandwidth *FCTokenBucket `json:"bandwidth,omitempty"`
	Ops       *FCTokenBucket `json:"ops,omitempty"`
}

// FCTokenBucket is used inside FCRateLimiter.
type FCTokenBucket struct {
	Size       int64 `json:"size"`
	OneTimeBurst int64 `json:"one_time_burst,omitempty"`
	RefillTime int64 `json:"refill_time"`
}

// FCNetworkInterface configures a TAP-backed network device.
type FCNetworkInterface struct {
	IfaceID           string      `json:"iface_id"`
	GuestMac          string      `json:"guest_mac,omitempty"`
	HostDevName       string      `json:"host_dev_name"`
	RxRateLimiter     *FCRateLimiter `json:"rx_rate_limiter,omitempty"`
	TxRateLimiter     *FCRateLimiter `json:"tx_rate_limiter,omitempty"`
	AllowMMDS         bool        `json:"allow_mmds_requests,omitempty"`
}

// FCVsock configures the virtio-vsock device.
// On the host side Firecracker creates a UDS at UDSPath; for each guest port
// connection the host-visible socket is UDSPath + "_" + port.
type FCVsock struct {
	VsockID  string `json:"vsock_id"`
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// FCLogger configures Firecracker's log output.
type FCLogger struct {
	LogPath       string `json:"log_path"`
	Level         string `json:"level,omitempty"`  // "Error" | "Warning" | "Info" | "Debug"
	ShowLevel     bool   `json:"show_level,omitempty"`
	ShowLogOrigin bool   `json:"show_log_origin,omitempty"`
}

// FCMetrics configures Firecracker's metrics FIFO output.
type FCMetrics struct {
	MetricsPath string `json:"metrics_path"`
}

// FCActionType enumerates the action types accepted by PUT /actions.
type FCActionType string

const (
	FCActionInstanceStart  FCActionType = "InstanceStart"
	FCActionSendCtrlAltDel FCActionType = "SendCtrlAltDel"
	FCActionFlushMetrics   FCActionType = "FlushMetrics"
)

// FCInstanceAction is the body for PUT /actions.
type FCInstanceAction struct {
	ActionType FCActionType `json:"action_type"`
}

// FCVMState is used for PATCH /vm to pause or resume.
type FCVMState struct {
	State string `json:"state"` // "Paused" | "Resumed"
}

// Normalised VM state strings returned by FCRuntime.GetState.
const (
	FCStateNotStarted = "Not started"
	FCStateRunning    = "Running"
	FCStatePaused     = "Paused"
)
