package compute

import "sync"

// HostConfig holds host-level binary paths shared by stateless plugins.
type HostConfig struct {
	CHBinary      string
	FCBinary      string
	FCJailerPath  string
	FCJailUID     int
	FCJailGID     int
	FCChrootBase  string
	KernelPath    string
	InitrdPath    string
	BaseImagesDir string
	InstancesDir  string
}

var (
	hostMu     sync.RWMutex
	hostConfig HostConfig
)

func SetHostConfig(cfg HostConfig) {
	hostMu.Lock()
	hostConfig = cfg
	hostMu.Unlock()
}

func Host() HostConfig {
	hostMu.RLock()
	defer hostMu.RUnlock()
	return hostConfig
}
