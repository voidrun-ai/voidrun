package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotateNumberedLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")
	if err := os.WriteFile(path, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".2", []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rotateNumberedLogs(path, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("current log should have been renamed")
	}
	got1, _ := os.ReadFile(path + ".1")
	if string(got1) != "current" {
		t.Fatalf(".1 = %q, want current", got1)
	}
	got2, _ := os.ReadFile(path + ".2")
	if string(got2) != "one" {
		t.Fatalf(".2 = %q, want one", got2)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("maxFiles=3 should drop the old .2")
	}
}

func TestRotatingWriterCapsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")
	w, err := newRotatingWriter(path, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, chunk := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "dddd" {
		t.Fatalf("current = %q, want dddd", cur)
	}
	one, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != "cccc" {
		t.Fatalf(".1 = %q, want cccc", one)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("expected at most 3 files")
	}
}

func TestConsolePumpWritesLog(t *testing.T) {
	prev := InstancesRoot
	InstancesRoot = t.TempDir()
	t.Cleanup(func() { InstancesRoot = prev })

	id := "pumptest"
	ensureConsoleLog(id)
	t.Cleanup(func() { StopConsolePump(id) })

	fifo, err := os.OpenFile(GetConsoleFifoPath(id), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fifo.Write([]byte("hello-hvc0\n")); err != nil {
		t.Fatal(err)
	}
	_ = fifo.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		b, err := os.ReadFile(GetConsoleLogPath(id))
		if err == nil && string(b) == "hello-hvc0\n" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("console.log = %q err=%v, want hello-hvc0", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
