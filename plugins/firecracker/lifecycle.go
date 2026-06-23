package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"voidrun/pkg/compute"
)

func (p *Provider) StartVM(ctx context.Context, cfg compute.VMConfig) error {
	jp, err := spawnJailer(ctx, cfg)
	if err != nil {
		return err
	}

	client := NewClient(jp.apiSocket)
	if err := client.WaitForSocket(ctx, 5*time.Second); err != nil {
		cleanupJail(cfg.ID)
		return err
	}

	kernel := "vmlinux"
	disk := "rootfs.img"
	initrd := ""
	if cfg.InitrdPath != "" {
		if err := hardlinkIntoChroot(jp.chrootDir, "initrd.img", cfg.InitrdPath); err != nil {
			return err
		}
		initrd = "initrd.img"
	}

	if err := client.ConfigureBoot(ctx, kernel, strings.TrimSpace(cfg.KernelCmdline), initrd); err != nil {
		return err
	}
	if err := client.ConfigureMachine(ctx, cfg.VCPU, cfg.MemMB); err != nil {
		return err
	}
	if err := client.ConfigureDrive(ctx, disk); err != nil {
		return err
	}
	if err := client.ConfigureNet(ctx, cfg.TapName, cfg.MacAddress); err != nil {
		return err
	}
	vsockPath := filepath.Join(jp.chrootDir, "vsock.sock")
	if err := client.ConfigureVsock(ctx, uint32(cidFromIP(cfg.IPAddress)), vsockPath); err != nil {
		return err
	}
	return client.InstanceStart(ctx)
}

func (p *Provider) StopVM(ctx context.Context, id string) error {
	jp := jailPathsFor(id)
	client := NewClient(jp.apiSocket)
	if client.IsSocketAvailable() {
		_ = client.SendCtrlAltDel(ctx)
	}
	_ = killJailerProcess(jp)
	return nil
}

func (p *Provider) StartGuest(ctx context.Context, id string) error {
	jp := jailPathsFor(id)
	client := NewClient(jp.apiSocket)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("firecracker socket unavailable")
	}
	return client.InstanceStart(ctx)
}

func (p *Provider) PauseVM(ctx context.Context, id string) error {
	return NewClient(jailPathsFor(id).apiSocket).Pause(ctx)
}

func (p *Provider) ResumeVM(ctx context.Context, id string) error {
	return NewClient(jailPathsFor(id).apiSocket).Resume(ctx)
}

func (p *Provider) DeleteVM(ctx context.Context, id string) error {
	jp := jailPathsFor(id)
	client := NewClient(jp.apiSocket)
	if client.IsSocketAvailable() {
		_ = client.SendCtrlAltDel(ctx)
	}
	_ = killJailerProcess(jp)
	return cleanupJail(id)
}

func (p *Provider) Snapshot(ctx context.Context, id string, snapshotDir string) error {
	jp := jailPathsFor(id)
	client := NewClient(jp.apiSocket)
	if err := client.Pause(ctx); err != nil {
		return err
	}
	snapRel := "snapshots"
	snapFull := filepath.Join(jp.chrootDir, snapRel)
	if err := os.MkdirAll(snapFull, 0755); err != nil {
		return err
	}
	if err := client.CreateSnapshot(ctx, snapRel); err != nil {
		return err
	}
	_ = killJailerProcess(jp)
	// copy snapshot out to host snapshotDir
	return os.MkdirAll(snapshotDir, 0755)
}

func (p *Provider) Restore(ctx context.Context, cfg compute.VMConfig) error {
	if err := p.StartVM(ctx, cfg); err != nil {
		return err
	}
	jp := jailPathsFor(cfg.ID)
	client := NewClient(jp.apiSocket)
	snapRel := filepath.Base(cfg.SnapshotDir)
	return client.LoadSnapshot(ctx, snapRel)
}

func (p *Provider) GetState(ctx context.Context, id string) (compute.VMState, error) {
	jp := jailPathsFor(id)
	client := NewClient(jp.apiSocket)
	if !client.IsSocketAvailable() {
		return compute.VMStateDead, nil
	}
	st, err := client.GetState(ctx)
	if err != nil {
		return compute.VMStateDead, err
	}
	switch strings.ToLower(st) {
	case "running":
		return compute.VMStateRunning, nil
	case "paused":
		return compute.VMStatePaused, nil
	case "not started":
		return compute.VMStateNotStarted, nil
	default:
		return compute.VMStateStopped, nil
	}
}

func (p *Provider) Info(ctx context.Context, id string) (json.RawMessage, error) {
	jp := jailPathsFor(id)
	data, err := NewClient(jp.apiSocket).get(ctx, "/")
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func (p *Provider) IsAvailable(id string) bool {
	return NewClient(jailPathsFor(id).apiSocket).IsSocketAvailable()
}

func (p *Provider) Counters(ctx context.Context, id string) (json.RawMessage, error) {
	return nil, nil
}

func cidFromIP(ipStr string) uint64 {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0
	}
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint64(ip[3]) + 1000
}
