package service

import (
	"sync"
	"testing"
)

func TestConnTracker_AcquireRelease(t *testing.T) {
	ct := NewConnTracker()

	ct.Acquire("sb1")
	if got := ct.ActiveCount("sb1"); got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}

	ct.Acquire("sb1")
	if got := ct.ActiveCount("sb1"); got != 2 {
		t.Fatalf("expected 2, got %d", got)
	}

	ct.Release("sb1")
	if got := ct.ActiveCount("sb1"); got != 1 {
		t.Fatalf("expected 1 after release, got %d", got)
	}

	ct.Release("sb1")
	if got := ct.ActiveCount("sb1"); got != 0 {
		t.Fatalf("expected 0 after second release, got %d", got)
	}
}

func TestConnTracker_ReleaseNeverNegative(t *testing.T) {
	ct := NewConnTracker()

	ct.Release("sb1") // release without acquire
	if got := ct.ActiveCount("sb1"); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestConnTracker_IsIdle(t *testing.T) {
	ct := NewConnTracker()

	if !ct.IsIdle("sb1") {
		t.Fatal("new sandbox should be idle")
	}

	ct.Acquire("sb1")
	if ct.IsIdle("sb1") {
		t.Fatal("should not be idle with active connection")
	}

	ct.Release("sb1")
	if !ct.IsIdle("sb1") {
		t.Fatal("should be idle after release")
	}
}

func TestConnTracker_MultipleSandboxes(t *testing.T) {
	ct := NewConnTracker()

	ct.Acquire("sb1")
	ct.Acquire("sb2")
	ct.Acquire("sb2")

	if got := ct.ActiveCount("sb1"); got != 1 {
		t.Fatalf("sb1: expected 1, got %d", got)
	}
	if got := ct.ActiveCount("sb2"); got != 2 {
		t.Fatalf("sb2: expected 2, got %d", got)
	}
	if got := ct.ActiveCount("sb3"); got != 0 {
		t.Fatalf("sb3: expected 0, got %d", got)
	}
}

func TestConnTracker_Remove(t *testing.T) {
	ct := NewConnTracker()

	ct.Acquire("sb1")
	ct.Remove("sb1")

	if got := ct.ActiveCount("sb1"); got != 0 {
		t.Fatalf("expected 0 after remove, got %d", got)
	}
}

func TestConnTracker_Concurrent(t *testing.T) {
	ct := NewConnTracker()
	const goroutines = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ct.Acquire("sb1")
		}()
	}
	wg.Wait()

	if got := ct.ActiveCount("sb1"); got != int64(goroutines) {
		t.Fatalf("expected %d, got %d", goroutines, got)
	}

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ct.Release("sb1")
		}()
	}
	wg.Wait()

	if got := ct.ActiveCount("sb1"); got != 0 {
		t.Fatalf("expected 0 after all releases, got %d", got)
	}
}
