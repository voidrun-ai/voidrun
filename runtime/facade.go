package runtime

import (
	"context"
	"fmt"
	"log"

	"voidrun/config"
	"voidrun/model"
	"voidrun/pkg/compute"
)

var configDefaultHypervisor = func() string { return "" }

// SetDefaultHypervisor is called from server setup with config value.
func SetDefaultHypervisor(name string) {
	configDefaultHypervisor = func() string { return name }
}

func BuildVMConfig(cfg config.Config, spec model.SandboxSpec, overlayPath string) compute.VMConfig {
	format := compute.FormatQcow2
	switch cfg.Sandbox.DiskFormat {
	case "raw":
		format = compute.FormatRaw
	case "qcow2-flat":
		format = compute.FormatQcow2Flat
	}

	kernel := cfg.Paths.KernelPath
	initrd := cfg.Paths.InitrdPath

	return compute.VMConfig{
		ID:             spec.ID,
		VCPU:           spec.CPUs,
		MemMB:          spec.MemoryMB,
		KernelPath:     kernel,
		InitrdPath:     initrd,
		KernelCmdline:  cfg.Sandbox.KernelCmdline,
		ImageType:      spec.Type,
		RootVolume:     compute.Volume{Path: overlayPath, Format: format},
		EnableSecurity: cfg.Sandbox.Seccomp,
		InstanceDir:    compute.GetInstanceDir(spec.ID),
		NetNSName:      spec.NetNSName,
		TapName:        spec.TapName,
		MacAddress:     spec.MacAddress,
		IPAddress:      spec.IPAddress,
		OverlayPath:    overlayPath,
		DebugConsole:   cfg.Sandbox.DebugBootConsole,
	}
}

func getHV(hvType string) (compute.Hypervisor, error) {
	if hvType == "" {
		hvType = configDefaultHypervisor()
	}
	t := string(compute.ResolveType(hvType, configDefaultHypervisor()))
	return compute.Get(t)
}

func CreateCLI(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	vmCfg := BuildVMConfig(cfg, spec, overlayPath)
	if err := validateDiskForHV(hv.Name(), vmCfg); err != nil {
		return err
	}
	return hv.StartVM(ctx, vmCfg)
}

func Create(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	vmCfg := BuildVMConfig(cfg, spec, overlayPath)
	if ch, ok := hv.(interface {
		ColdBoot(context.Context, compute.VMConfig) error
	}); ok {
		return ch.ColdBoot(ctx, vmCfg)
	}
	return hv.StartVM(ctx, vmCfg)
}

func Stop(ctx context.Context, id, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	return hv.StopVM(ctx, id)
}

func Start(ctx context.Context, id, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	return hv.StartGuest(ctx, id)
}

func Pause(ctx context.Context, id, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	return hv.PauseVM(ctx, id)
}

func Resume(ctx context.Context, id, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	return hv.ResumeVM(ctx, id)
}

func Snapshot(ctx context.Context, id, snapshotDir, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	return hv.Snapshot(ctx, id, snapshotDir)
}

func Restore(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath, snapshotDir, hvType string) error {
	if err := EnsureSandboxNetNS(cfg, &spec); err != nil {
		return fmt.Errorf("ensure netns: %w", err)
	}
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	vmCfg := BuildVMConfig(cfg, spec, overlayPath)
	vmCfg.SnapshotDir = snapshotDir
	if err := hv.Restore(ctx, vmCfg); err != nil {
		return err
	}
	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] EnsureTapBridge failed after restore: %v", err)
	}
	return nil
}

func DeleteVM(ctx context.Context, id, hvType string) error {
	hv, err := getHV(hvType)
	if err != nil {
		return err
	}
	return hv.DeleteVM(ctx, id)
}

func Info(ctx context.Context, id, hvType string) (string, error) {
	hv, err := getHV(hvType)
	if err != nil {
		return "", err
	}
	raw, err := hv.Info(ctx, id)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func GetState(ctx context.Context, id, hvType string) (compute.VMState, error) {
	hv, err := getHV(hvType)
	if err != nil {
		return compute.VMStateDead, err
	}
	return hv.GetState(ctx, id)
}

func IsSocketAvailable(id, hvType string) bool {
	hv, err := getHV(hvType)
	if err != nil {
		return false
	}
	return hv.IsAvailable(id)
}

func validateDiskForHV(hvName string, cfg compute.VMConfig) error {
	if hvName != string(compute.TypeFirecracker) {
		return nil
	}
	if cfg.RootVolume.Format == compute.FormatQcow2 {
		return fmt.Errorf("firecracker does not support qcow2 backing overlays; use raw or qcow2-flat")
	}
	return nil
}

// SyncHostConfig publishes paths to plugin packages.
func SyncHostConfig(cfg *config.Config) {
	compute.SetHostConfig(compute.HostConfig{
		CHBinary:      cfg.CHBinary,
		FCBinary:      cfg.FCBinary,
		FCJailerPath:  cfg.FCJailerPath,
		FCJailUID:     cfg.Hypervisor.FCJailUID,
		FCJailGID:     cfg.Hypervisor.FCJailGID,
		FCChrootBase:  cfg.Hypervisor.FCChrootBase,
		KernelPath:    cfg.Paths.KernelPath,
		InitrdPath:    cfg.Paths.InitrdPath,
		BaseImagesDir: cfg.Paths.BaseImagesDir,
		InstancesDir:  cfg.Paths.InstancesDir,
	})
	compute.SetInstancesRoot(cfg.Paths.InstancesDir)
	SetInstancesRoot(cfg.Paths.InstancesDir)
	SetDefaultHypervisor(cfg.Hypervisor.Default)
}
