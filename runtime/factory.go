package runtime

import (
	"fmt"
	"voidrun/config"
)

// NewRuntime constructs the HypervisorRuntime appropriate for cfg.
// The caller is responsible for passing the correct config; the factory does
// not validate binary paths or kernel images at construction time.
//
// Supported values for cfg.HypervisorType:
//
//	"cloud-hypervisor"  (default / empty) → CLHRuntime
//	"firecracker"                          → FCRuntime
func NewRuntime(cfg *config.Config) (HypervisorRuntime, error) {
	switch cfg.HypervisorType {
	case string(HypervisorCloudHypervisor), "":
		return NewCLHRuntime(), nil
	case string(HypervisorFirecracker):
		return NewFCRuntime(), nil
	default:
		return nil, fmt.Errorf("unknown hypervisor type %q (use \"cloud-hypervisor\" or \"firecracker\")", cfg.HypervisorType)
	}
}

// NewRuntimeForSandbox returns the HypervisorRuntime that was used to create a
// specific sandbox instance.  It reads the vm.hypervisor marker file written to
// disk at Create time, falling back to Cloud Hypervisor for sandboxes created
// before this field was introduced.
func NewRuntimeForSandbox(sandboxID string) HypervisorRuntime {
	hvType := ReadHypervisorType(sandboxID)
	switch HypervisorType(hvType) {
	case HypervisorFirecracker:
		return NewFCRuntime()
	default:
		return NewCLHRuntime()
	}
}

// WriteHypervisorType persists the hypervisor type to disk inside the sandbox
// instance directory.  This file is created once at sandbox creation time and
// read by NewRuntimeForSandbox and DialVsock to select the correct protocol.
func WriteHypervisorType(sandboxID string, ht HypervisorType) error {
	path := GetHypervisorTypePath(sandboxID)
	return writeFileAtomic(path, []byte(string(ht)))
}
