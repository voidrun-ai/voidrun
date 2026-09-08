package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"voidrun/config"
	"voidrun/model"
)

func TestGetConsoleLogPath(t *testing.T) {
	prev := InstancesRoot
	InstancesRoot = "/var/lib/voidrun/instances"
	t.Cleanup(func() { InstancesRoot = prev })

	got := GetConsoleLogPath("abc123")
	want := "/var/lib/voidrun/instances/abc123/console.log"
	if got != want {
		t.Fatalf("GetConsoleLogPath = %q, want %q", got, want)
	}
}

func TestBuildCLIArgsConsoleLogFile(t *testing.T) {
	prevRoot := InstancesRoot
	prevDecoupled := DecoupledSnapshotEnabled
	InstancesRoot = t.TempDir()
	DecoupledSnapshotEnabled = false
	t.Cleanup(func() {
		InstancesRoot = prevRoot
		DecoupledSnapshotEnabled = prevDecoupled
	})

	id := "sbxconsole1"
	overlay := filepath.Join(InstancesRoot, id, "overlay.qcow2")
	cfg := config.Config{
		Paths: config.PathsConfig{
			KernelPath:    "/tmp/vmlinux",
			BaseImagesDir: "/tmp/base-images",
		},
		Sandbox: config.SandboxConfig{
			KernelCmdline: "root=/dev/vda rw",
			DiskFormat:    "qcow2",
		},
	}
	spec := model.SandboxSpec{
		ID:         id,
		Type:       "code",
		CPUs:       1,
		MemoryMB:   512,
		IPAddress:  "10.0.0.5",
		TapName:    "tap0",
		MacAddress: "aa:bb:cc:dd:ee:ff",
		NetNSName:  "ns0",
	}

	args := BuildCLIArgs(cfg, spec, overlay)
	if got := cliFlag(args, "--serial"); got != "off" {
		t.Fatalf("--serial = %q, want off", got)
	}
	if got := cliFlag(args, "--console"); got != "off" {
		t.Fatalf("default --console = %q, want off", got)
	}

	spec.ConsoleLogEnabled = true
	args = BuildCLIArgs(cfg, spec, overlay)
	if got := cliFlag(args, "--serial"); got != "off" {
		t.Fatalf("--serial = %q, want off", got)
	}
	wantConsole := "file=" + GetConsoleFifoPath(id)
	if got := cliFlag(args, "--console"); got != wantConsole {
		t.Fatalf("--console = %q, want %q", got, wantConsole)
	}

	cfg.Sandbox.DebugBootConsole = true
	args = BuildCLIArgs(cfg, spec, overlay)
	if got := cliFlag(args, "--serial"); got != "tty" {
		t.Fatalf("debug --serial = %q, want tty", got)
	}
	if got := cliFlag(args, "--console"); got != wantConsole {
		t.Fatalf("debug --console = %q, want %q", got, wantConsole)
	}

	DecoupledSnapshotEnabled = true
	args = BuildCLIArgs(cfg, spec, overlay)
	if got := cliFlag(args, "--serial"); got != "tty" {
		t.Fatalf("decoupled debug --serial = %q, want tty", got)
	}
	if got := cliFlag(args, "--console"); got != wantConsole {
		t.Fatalf("decoupled --console = %q, want %q", got, wantConsole)
	}
	cfg.Sandbox.DebugBootConsole = false
	args = BuildCLIArgs(cfg, spec, overlay)
	if got := cliFlag(args, "--serial"); got != "off" {
		t.Fatalf("decoupled --serial = %q, want off", got)
	}
	if got := cliFlag(args, "--console"); got != wantConsole {
		t.Fatalf("decoupled --console = %q, want %q", got, wantConsole)
	}
}

func TestEnsureConsoleLog(t *testing.T) {
	prev := InstancesRoot
	InstancesRoot = t.TempDir()
	t.Cleanup(func() { InstancesRoot = prev })

	id := "sbxconsole2"
	dir := GetInstanceDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ensureConsoleLog(id)
	t.Cleanup(func() { StopConsolePump(id) })
	st, err := os.Lstat(GetConsoleFifoPath(id))
	if err != nil {
		t.Fatalf("console.fifo missing: %v", err)
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("console.fifo mode %v, want named pipe", st.Mode())
	}
}

func TestPrepareConsoleLogOffNoFifo(t *testing.T) {
	prev := InstancesRoot
	InstancesRoot = t.TempDir()
	t.Cleanup(func() { InstancesRoot = prev })

	id := "sbxconsole3"
	dir := GetInstanceDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	prepareConsoleLog(model.SandboxSpec{ID: id})
	if _, err := os.Lstat(GetConsoleFifoPath(id)); !os.IsNotExist(err) {
		t.Fatalf("console.fifo should be absent when virtio is off, err=%v", err)
	}
}

func cliFlag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
