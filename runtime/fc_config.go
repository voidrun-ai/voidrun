package runtime

// Firecracker REST API types.
// Reference: https://github.com/firecracker-microvm/firecracker/blob/main/src/api_server/swagger/firecracker.yaml

// FCMachineConfig is the body for PUT /machine-config.
type FCMachineConfig struct {
	VcpuCount  int  `json:"vcpu_count"`
	MemSizeMib int  `json:"mem_size_mib"`
	SMT        bool `json:"smt,omitempty"`
	TrackDirtyPages bool `json:"track_dirty_pages,omitempty"`
}

// FCBootSource is the body for PUT /boot-source.
type FCBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// FCDrive is the body for PUT /drives/{drive_id}.
type FCDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	// RateLimiter omitted for simplicity
}

// FCNetworkInterface is the body for PUT /network-interfaces/{iface_id}.
type FCNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	// RxRateLimiter / TxRateLimiter omitted for simplicity
}

// FCVsock is the body for PUT /vsock.
type FCVsock struct {
	GuestCID uint64 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// FCLogger is the body for PUT /logger.
type FCLogger struct {
	LogPath     string `json:"log_path"`
	Level       string `json:"level,omitempty"`       // Error, Warning, Info, Debug
	ShowLevel   bool   `json:"show_level,omitempty"`
	ShowLogOrigin bool `json:"show_log_origin,omitempty"`
}

// FCAction is the body for PUT /actions.
type FCAction struct {
	ActionType string `json:"action_type"`
}

// Firecracker action type constants.
const (
	FCActionInstanceStart  = "InstanceStart"
	FCActionSendCtrlAltDel = "SendCtrlAltDel"
	FCActionFlushMetrics    = "FlushMetrics"
)

// FCVMState is the body for PATCH /vm.
type FCVMState struct {
	State string `json:"state"`
}

// Firecracker VM state constants for PATCH /vm.
const (
	FCVMStatePaused  = "Paused"
	FCVMStateResumed = "Resumed"
)

// FCInstanceInfo is the response body for GET /.
type FCInstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	VMMVersion string `json:"vmm_version"`
	AppName    string `json:"app_name"`
}

// Firecracker instance state strings (as reported by GET /).
const (
	FCStateNotStarted = "Not started"
	FCStateRunning    = "Running"
	FCStatePaused     = "Paused"
)
