//go:build linux

package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWatchPIDSeesExit(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	done := make(chan error, 1)
	go func() { done <- WatchPID(cmd.Process.Pid, nil) }()

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchPID: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchPID did not return after kill")
	}
}

func TestWatchPIDStop(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- WatchPID(cmd.Process.Pid, stop) }()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	close(stop)

	select {
	case err := <-done:
		if err != ErrWatchStopped {
			t.Fatalf("WatchPID = %v, want ErrWatchStopped", err)
		}
		if time.Since(start) > 200*time.Millisecond {
			t.Fatalf("stop took %s, epoll cancel should be immediate", time.Since(start))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchPID did not return after stop")
	}
}

func TestWatchPIDAlreadyDead(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()

	done := make(chan error, 1)
	go func() { done <- WatchPID(pid, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchPID dead pid: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchPID blocked on reaped pid")
	}
}

func TestWatchPIDMany(t *testing.T) {
	const n = 64
	cmds := make([]*exec.Cmd, n)
	dones := make([]chan error, n)
	for i := 0; i < n; i++ {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		cmds[i] = cmd
		d := make(chan error, 1)
		dones[i] = d
		go func(pid int, d chan error) { d <- WatchPID(pid, nil) }(cmd.Process.Pid, d)
	}
	t.Cleanup(func() {
		for _, cmd := range cmds {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	for _, cmd := range cmds {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill: %v", err)
		}
	}
	deadline := time.After(3 * time.Second)
	for i, d := range dones {
		select {
		case err := <-d:
			if err != nil {
				t.Fatalf("watch %d: %v", i, err)
			}
		case <-deadline:
			t.Fatalf("watch %d did not return", i)
		}
	}
}

func TestLiveCHPIDMissingFile(t *testing.T) {
	if pid, ok := LiveCHPID("no-such-sandbox"); ok || pid != 0 {
		t.Fatalf("LiveCHPID = %d, %v, want 0, false", pid, ok)
	}
}

func TestLiveCHPIDSeesLivePID(t *testing.T) {
	prev := InstancesRoot
	InstancesRoot = t.TempDir()
	t.Cleanup(func() { InstancesRoot = prev })

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	id := "sbx-live"
	if err := os.MkdirAll(filepath.Join(InstancesRoot, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GetPIDPath(id), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	pid, ok := LiveCHPID(id)
	if !ok || pid != cmd.Process.Pid {
		t.Fatalf("LiveCHPID = %d, %v, want %d, true", pid, ok, cmd.Process.Pid)
	}
}

func TestLiveCHPIDDeadPID(t *testing.T) {
	prev := InstancesRoot
	InstancesRoot = t.TempDir()
	t.Cleanup(func() { InstancesRoot = prev })

	id := "sbx-dead"
	if err := os.MkdirAll(filepath.Join(InstancesRoot, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GetPIDPath(id), []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid, ok := LiveCHPID(id); ok || pid != 0 {
		t.Fatalf("LiveCHPID dead = %d, %v, want 0, false", pid, ok)
	}
}
