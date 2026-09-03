package runtime

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Host RAM backing modes: legacy, shared-shm (/dev/shm), private-tmpfs (per-sandbox tmpfs).
const (
	MemBackingLegacy       = "legacy"
	MemBackingSharedShm    = "shared-shm"
	MemBackingPrivateTmpfs = "private-tmpfs"

	parkedRAMFileName  = "mem.raw"
	sharedShmDir       = "/dev/shm"
	sharedShmRAMPrefix = "voidrun-"
	sharedShmRAMSuffix = ".ram"
	privateTmpfsSubdir = "mem"
	privateTmpfsRAM    = "ram.raw"
	ramFilePerm        = 0o600
	ramDirPerm         = 0o700
)

// GetRAMFilePath returns the live guest-RAM backing file path for the current mode.
func GetRAMFilePath(sandboxID string) string {
	if MemoryBackingModeName == MemBackingPrivateTmpfs {
		return filepath.Join(GetInstanceDir(sandboxID), privateTmpfsSubdir, privateTmpfsRAM)
	}
	return filepath.Join(sharedShmDir, sharedShmRAMPrefix+sandboxID+sharedShmRAMSuffix)
}

// GetParkedRAMPath is the snapshot-dir path for parked guest RAM (always on instance FS).
func GetParkedRAMPath(snapshotDir string) string {
	return filepath.Join(snapshotDir, parkedRAMFileName)
}

// EnsureRAMFile creates/truncates the RAM file (0600); mounts tmpfs for private-tmpfs.
func EnsureRAMFile(sandboxID string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("EnsureRAMFile: invalid size %d", sizeBytes)
	}
	if MemoryBackingModeName == MemBackingPrivateTmpfs {
		mountDir := filepath.Join(GetInstanceDir(sandboxID), privateTmpfsSubdir)
		if err := ensureTmpfsMount(mountDir, sizeBytes); err != nil {
			return fmt.Errorf("EnsureRAMFile private-tmpfs: %w", err)
		}
	}
	return createRAMFile(GetRAMFilePath(sandboxID), sizeBytes)
}

// createRAMFile creates, chmods, and truncates the RAM file.
func createRAMFile(path string, sizeBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(path), ramDirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, ramFilePerm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := os.Chmod(path, ramFilePerm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		return fmt.Errorf("truncate %s to %d: %w", path, sizeBytes, err)
	}
	return nil
}

// EvictRAMToSnapshot sparse-moves live RAM into the snapshot dir after VMM exit.
func EvictRAMToSnapshot(sandboxID, snapshotDir string) error {
	src := GetRAMFilePath(sandboxID)
	dst := GetParkedRAMPath(snapshotDir)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("EvictRAMToSnapshot: live RAM file missing at %s", src)
		}
		return fmt.Errorf("EvictRAMToSnapshot stat %s: %w", src, err)
	}
	_ = os.Remove(dst)
	if err := SparseMove(src, dst); err != nil {
		return fmt.Errorf("EvictRAMToSnapshot: %w", err)
	}
	if err := os.Chmod(dst, ramFilePerm); err != nil {
		return fmt.Errorf("EvictRAMToSnapshot chmod parked %s: %w", dst, err)
	}
	if MemoryBackingModeName == MemBackingPrivateTmpfs {
		mountDir := filepath.Join(GetInstanceDir(sandboxID), privateTmpfsSubdir)
		if err := umountTmpfs(mountDir); err != nil {
			log.Printf("[WARN] EvictRAMToSnapshot umount %s: %v", mountDir, err)
		}
	}
	return nil
}

// RestoreRAMFromSnapshot copies parked RAM back to the live backing path.
func RestoreRAMFromSnapshot(sandboxID, snapshotDir string) error {
	src := GetParkedRAMPath(snapshotDir)
	dst := GetRAMFilePath(sandboxID)
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("RestoreRAMFromSnapshot: parked RAM missing at %s: %w", src, err)
	}
	// Best-effort readahead hint before sparse copy.
	prefetchFile(src)
	if MemoryBackingModeName == MemBackingPrivateTmpfs {
		mountDir := filepath.Join(GetInstanceDir(sandboxID), privateTmpfsSubdir)
		if err := ensureTmpfsMount(mountDir, st.Size()); err != nil {
			return fmt.Errorf("RestoreRAMFromSnapshot mount %s: %w", mountDir, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), ramDirPerm); err != nil {
			return fmt.Errorf("RestoreRAMFromSnapshot mkdir %s: %w", filepath.Dir(dst), err)
		}
	}
	_ = os.Remove(dst)
	if err := SparseCopy(src, dst); err != nil {
		return fmt.Errorf("RestoreRAMFromSnapshot: %w", err)
	}
	if err := os.Chmod(dst, ramFilePerm); err != nil {
		return fmt.Errorf("RestoreRAMFromSnapshot chmod %s: %w", dst, err)
	}
	return nil
}

// prefetchFile issues POSIX_FADV_WILLNEED; failures are ignored.
func prefetchFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_WILLNEED)
}

// RemoveRAMFile removes the live RAM file and unmounts private-tmpfs if needed.
func RemoveRAMFile(sandboxID string) error {
	path := GetRAMFilePath(sandboxID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("RemoveRAMFile %s: %w", path, err)
	}
	if MemoryBackingModeName == MemBackingPrivateTmpfs {
		mountDir := filepath.Join(GetInstanceDir(sandboxID), privateTmpfsSubdir)
		if err := umountTmpfs(mountDir); err != nil {
			log.Printf("[WARN] RemoveRAMFile umount %s: %v", mountDir, err)
		}
		_ = os.Remove(mountDir) // rmdir only; ignore ENOTEMPTY
	}
	return nil
}

// ensureTmpfsMount mounts a sized per-sandbox tmpfs; no-op if already mounted.
func ensureTmpfsMount(dir string, sizeBytes int64) error {
	if err := os.MkdirAll(dir, ramDirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, ramDirPerm); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	mounted, err := isMountpoint(dir)
	if err != nil {
		return fmt.Errorf("isMountpoint %s: %w", dir, err)
	}
	if mounted {
		return nil
	}
	opts := tmpfsMountOpts(sizeBytes, MemoryAllowSwap)
	if MemoryAllowSwap {
		log.Printf("[memory] mounting %s without noswap (SANDBOX_RAM_ALLOW_SWAP)", dir)
	}
	cmd := exec.Command("mount", "-t", "tmpfs", "-o", opts, "voidrun-ram", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		// noswap needs Linux ≥6.4; mount without it on older kernels.
		outStr := string(out)
		if !MemoryAllowSwap && (strings.Contains(outStr, "unrecognized") ||
			strings.Contains(outStr, "bad option") ||
			strings.Contains(outStr, "unknown option")) {
			log.Printf("[WARN] tmpfs mount rejected `noswap` on %s (kernel <6.4?); mounting WITHOUT noswap — guest RAM may reach host swap. mount stderr: %s", dir, outStr)
			cmd = exec.Command("mount", "-t", "tmpfs", "-o", tmpfsMountOpts(sizeBytes, true), "voidrun-ram", dir)
			if out2, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("mount tmpfs %s: %v: %s", dir, err2, string(out2))
			}
			return nil
		}
		return fmt.Errorf("mount tmpfs %s: %v: %s", dir, err, outStr)
	}
	return nil
}

func tmpfsMountOpts(sizeBytes int64, allowSwap bool) string {
	opts := fmt.Sprintf("size=%d,mode=0700", sizeBytes)
	if !allowSwap {
		opts += ",noswap"
	}
	return opts
}

// umountTmpfs unmounts dir, falling back to lazy unmount on EBUSY.
func umountTmpfs(dir string) error {
	mounted, err := isMountpoint(dir)
	if err != nil || !mounted {
		return err
	}
	cmd := exec.Command("umount", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[WARN] umount %s failed (%v: %s); retrying lazy", dir, err, string(out))
		cmd = exec.Command("umount", "-l", dir)
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			return fmt.Errorf("lazy umount %s: %v: %s", dir, err2, string(out2))
		}
	}
	return nil
}

// isMountpoint reports whether dir is a mount root (device differs from parent).
func isMountpoint(dir string) (bool, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	parent, err := os.Stat(filepath.Dir(dir))
	if err != nil {
		return false, err
	}
	fiSt, ok1 := fi.Sys().(*syscall.Stat_t)
	parSt, ok2 := parent.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, fmt.Errorf("isMountpoint: unexpected Stat_t type")
	}
	return fiSt.Dev != parSt.Dev, nil
}
