package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"voidrun/pkg/compute"
)

type jailPaths struct {
	chrootDir string
	apiSocket string
	pidFile   string
}

func jailPathsFor(id string) jailPaths {
	host := compute.Host()
	base := host.FCChrootBase
	if base == "" {
		base = filepath.Join(host.InstancesDir, "jails")
	}
	return jailPaths{
		chrootDir: compute.GetFCChrootDir(base, id),
		apiSocket: compute.GetFCAPISocketPath(base, id),
		pidFile:   compute.GetFCPIDPath(base, id),
	}
}

func chrootBaseDir() string {
	host := compute.Host()
	if host.FCChrootBase != "" {
		return host.FCChrootBase
	}
	return filepath.Join(host.InstancesDir, "jails")
}

func hardlinkIntoChroot(chrootDir, name, src string) error {
	dst := filepath.Join(chrootDir, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Link(src, dst)
}

func prepareChrootAssets(chrootDir string, cfg compute.VMConfig) error {
	host := compute.Host()
	kernel := cfg.KernelPath
	if kernel == "" {
		kernel = host.KernelPath
	}
	disk := cfg.OverlayPath
	if disk == "" {
		disk = cfg.RootVolume.Path
	}
	if err := hardlinkIntoChroot(chrootDir, "vmlinux", kernel); err != nil {
		return fmt.Errorf("kernel: %w", err)
	}
	if err := hardlinkIntoChroot(chrootDir, "rootfs.img", disk); err != nil {
		return fmt.Errorf("disk: %w", err)
	}
	vsockHost := compute.GetVsockPath(cfg.ID)
	_ = os.MkdirAll(chrootDir, 0755)
	_ = hardlinkIntoChroot(chrootDir, "vsock.sock", vsockHost)
	return chownTree(chrootDir, host.FCJailUID, host.FCJailGID)
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}

func spawnJailer(ctx context.Context, cfg compute.VMConfig) (jailPaths, error) {
	host := compute.Host()
	if host.FCJailerPath == "" || host.FCBinary == "" {
		return jailPaths{}, fmt.Errorf("firecracker: FC_JAILER_PATH and FC_PATH required")
	}

	jp := jailPathsFor(cfg.ID)
	_ = os.RemoveAll(filepath.Dir(jp.chrootDir))

	netnsPath := filepath.Join("/var/run/netns", cfg.NetNSName)
	args := []string{
		"--id", cfg.ID,
		"--exec-file", host.FCBinary,
		"--uid", strconv.Itoa(host.FCJailUID),
		"--gid", strconv.Itoa(host.FCJailGID),
		"--chroot-base-dir", chrootBaseDir(),
		"--netns", netnsPath,
		"--daemonize",
		"--new-pid-ns",
		"--",
		"--api-sock", "run/firecracker.socket",
	}

	cmd := exec.CommandContext(ctx, host.FCJailerPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return jp, fmt.Errorf("jailer start: %w: %s", err, string(out))
	}

	// Jailer creates chroot; link assets after root exists
	if err := prepareChrootAssets(jp.chrootDir, cfg); err != nil {
		return jp, err
	}
	// Ensure API socket parent exists and is writable for the jailed UID.
	runDir := filepath.Join(jp.chrootDir, "run")
	_ = os.MkdirAll(runDir, 0755)
	_ = chownTree(runDir, host.FCJailUID, host.FCJailGID)
	return jp, nil
}

func killJailerProcess(jp jailPaths) error {
	data, err := os.ReadFile(jp.pidFile)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return proc.Signal(syscall.SIGKILL)
}

func cleanupJail(id string) error {
	return os.RemoveAll(filepath.Join(chrootBaseDir(), "firecracker", id))
}
