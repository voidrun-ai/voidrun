package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The tests below require CAP_SYS_ADMIN for the private-tmpfs paths
// (mount(2)). They auto-skip when unprivileged so `go test ./...` from a
// regular user still passes.

func requireRoot(t *testing.T) {
	t.Helper()
	if syscall.Geteuid() != 0 {
		t.Skip("test requires root (mount(2))")
	}
}

// pokeSparseWrite writes a small string at a large offset in a sparse
// file so we can assert SparseCopy preserves the hole. dd/seek is
// simulated with a File+Seek.
func pokeSparseWrite(t *testing.T, path string, offset int64, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readAt(t *testing.T, path string, offset int64, n int) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, n)
	if _, err := f.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

func statBlocks(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return st.Blocks
}

func TestSharedShmRoundTrip(t *testing.T) {
	requireRoot(t)

	prevRoot := InstancesRoot
	prevMode := MemoryBackingModeName
	InstancesRoot = t.TempDir()
	MemoryBackingModeName = MemBackingSharedShm
	t.Cleanup(func() {
		InstancesRoot = prevRoot
		MemoryBackingModeName = prevMode
	})

	id := "utest-shm-" + strings.ReplaceAll(t.Name(), "/", "-")
	const size = int64(16 * 1024 * 1024) // 16 MiB sparse

	t.Cleanup(func() { _ = RemoveRAMFile(id) })

	if err := EnsureRAMFile(id, size); err != nil {
		t.Fatalf("EnsureRAMFile: %v", err)
	}
	ramPath := GetRAMFilePath(id)
	fi, err := os.Stat(ramPath)
	if err != nil {
		t.Fatalf("stat ram file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("ram file perm = %o, want 0600", fi.Mode().Perm())
	}
	if fi.Size() != size {
		t.Fatalf("ram file apparent size = %d, want %d", fi.Size(), size)
	}
	if got := statBlocks(t, ramPath); got != 0 {
		t.Fatalf("freshly-truncated ram file should be fully sparse, got %d blocks", got)
	}

	pokeSparseWrite(t, ramPath, size-int64(len("hello world")), "hello world")

	snapDir := filepath.Join(t.TempDir(), "snap-shm")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EvictRAMToSnapshot(id, snapDir); err != nil {
		t.Fatalf("EvictRAMToSnapshot: %v", err)
	}
	if _, err := os.Stat(ramPath); !os.IsNotExist(err) {
		t.Fatalf("live ram file should be gone after evict, err=%v", err)
	}
	parked := GetParkedRAMPath(snapDir)
	if got := readAt(t, parked, size-int64(len("hello world")), 11); got != "hello world" {
		t.Fatalf("parked file lost marker: got %q", got)
	}

	if err := RestoreRAMFromSnapshot(id, snapDir); err != nil {
		t.Fatalf("RestoreRAMFromSnapshot: %v", err)
	}
	if got := readAt(t, ramPath, size-int64(len("hello world")), 11); got != "hello world" {
		t.Fatalf("restored file lost marker: got %q", got)
	}

	if err := RemoveRAMFile(id); err != nil {
		t.Fatalf("RemoveRAMFile: %v", err)
	}
	if _, err := os.Stat(ramPath); !os.IsNotExist(err) {
		t.Fatalf("ram file should be gone after RemoveRAMFile, err=%v", err)
	}
}

func TestPrivateTmpfsRoundTrip(t *testing.T) {
	requireRoot(t)

	prevRoot := InstancesRoot
	prevMode := MemoryBackingModeName
	InstancesRoot = t.TempDir()
	MemoryBackingModeName = MemBackingPrivateTmpfs
	t.Cleanup(func() {
		InstancesRoot = prevRoot
		MemoryBackingModeName = prevMode
	})

	id := "utest-tmpfs-" + strings.ReplaceAll(t.Name(), "/", "-")
	const size = int64(16 * 1024 * 1024)
	mountDir := filepath.Join(GetInstanceDir(id), "mem")

	t.Cleanup(func() { _ = RemoveRAMFile(id) })

	if err := EnsureRAMFile(id, size); err != nil {
		t.Fatalf("EnsureRAMFile: %v", err)
	}
	mounted, err := isMountpoint(mountDir)
	if err != nil || !mounted {
		t.Fatalf("expected %s to be a mountpoint, mounted=%v err=%v", mountDir, mounted, err)
	}
	ramPath := GetRAMFilePath(id)
	fi, err := os.Stat(ramPath)
	if err != nil {
		t.Fatalf("stat ram file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("ram file perm = %o, want 0600", fi.Mode().Perm())
	}

	// EnsureRAMFile semantics = "start fresh"; assert idempotency at the
	// mount layer (second call must not fail or remount) before poking.
	if err := EnsureRAMFile(id, size); err != nil {
		t.Fatalf("EnsureRAMFile idempotent: %v", err)
	}
	mounted, err = isMountpoint(mountDir)
	if err != nil || !mounted {
		t.Fatalf("mount lost across idempotent Ensure, mounted=%v err=%v", mounted, err)
	}

	pokeSparseWrite(t, ramPath, size-int64(len("tmpfs-marker")), "tmpfs-marker")

	snapDir := filepath.Join(t.TempDir(), "snap-tmpfs")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EvictRAMToSnapshot(id, snapDir); err != nil {
		t.Fatalf("EvictRAMToSnapshot: %v", err)
	}
	mounted, _ = isMountpoint(mountDir)
	if mounted {
		t.Fatalf("expected %s to be unmounted after evict", mountDir)
	}
	parked := GetParkedRAMPath(snapDir)
	if got := readAt(t, parked, size-int64(len("tmpfs-marker")), 12); got != "tmpfs-marker" {
		t.Fatalf("parked file lost marker: got %q", got)
	}

	if err := RestoreRAMFromSnapshot(id, snapDir); err != nil {
		t.Fatalf("RestoreRAMFromSnapshot: %v", err)
	}
	mounted, _ = isMountpoint(mountDir)
	if !mounted {
		t.Fatalf("expected %s to be mounted after restore", mountDir)
	}
	if got := readAt(t, ramPath, size-int64(len("tmpfs-marker")), 12); got != "tmpfs-marker" {
		t.Fatalf("restored file lost marker: got %q", got)
	}

	if err := RemoveRAMFile(id); err != nil {
		t.Fatalf("RemoveRAMFile: %v", err)
	}
	mounted, _ = isMountpoint(mountDir)
	if mounted {
		t.Fatalf("expected %s to be unmounted after RemoveRAMFile", mountDir)
	}
}

func TestSparseCopyPreservesHoles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	const size = int64(32 * 1024 * 1024) // 32 MiB apparent

	f, err := os.OpenFile(src, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("head"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tail"), size-4); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	srcBlocks := statBlocks(t, src)

	if err := SparseCopy(src, dst); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Fatalf("dst apparent size = %d, want %d", fi.Size(), size)
	}
	dstBlocks := statBlocks(t, dst)
	// Loose bound: dst should not be dramatically larger than src. Exact
	// equality varies by FS (tmpfs, ext4, xfs) and reflink availability.
	if dstBlocks > srcBlocks*4 {
		t.Fatalf("dst not sparse: %d blocks vs src %d blocks", dstBlocks, srcBlocks)
	}
	if readAt(t, dst, 0, 4) != "head" || readAt(t, dst, size-4, 4) != "tail" {
		t.Fatalf("content mismatch after copy")
	}
}

func TestTmpfsMountOpts(t *testing.T) {
	got := tmpfsMountOpts(1<<30, false)
	if got != "size=1073741824,mode=0700,noswap" {
		t.Fatalf("default opts = %q", got)
	}
	got = tmpfsMountOpts(1<<30, true)
	if got != "size=1073741824,mode=0700" {
		t.Fatalf("allow-swap opts = %q", got)
	}
}
