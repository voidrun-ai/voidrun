package service

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSandboxLifecycleLocks_MutualExclusionSameID(t *testing.T) {
	const goroutines = 50
	locks := NewSandboxLifecycleLocks()

	var active, maxActive int32
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			release := locks.Acquire("sbx-1")
			defer release()

			cur := atomic.AddInt32(&active, 1)
			for {
				prev := atomic.LoadInt32(&maxActive)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("max concurrent holders for same id = %d, want 1", got)
	}
	if n := locks.size(); n != 0 {
		t.Fatalf("locks map size after release = %d, want 0", n)
	}
}

func TestSandboxLifecycleLocks_NoContentionAcrossIDs(t *testing.T) {
	locks := NewSandboxLifecycleLocks()

	releaseA := locks.Acquire("sbx-a")
	defer releaseA()

	done := make(chan struct{})
	go func() {
		releaseB := locks.Acquire("sbx-b")
		releaseB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("acquiring a different id blocked while another id was held")
	}
}

func TestSandboxLifecycleLocks_MultipleHoldersSameID(t *testing.T) {
	locks := NewSandboxLifecycleLocks()

	releaseA := locks.Acquire("sbx-x")

	bAcquired := make(chan struct{})
	bReleased := make(chan struct{})
	go func() {
		releaseB := locks.Acquire("sbx-x")
		close(bAcquired)
		releaseB()
		close(bReleased)
	}()

	select {
	case <-bAcquired:
		t.Fatal("second acquire on same id proceeded while first was held")
	case <-time.After(50 * time.Millisecond):
	}

	if n := locks.size(); n != 1 {
		t.Fatalf("locks map size with one held + one waiter = %d, want 1", n)
	}

	releaseA()

	select {
	case <-bReleased:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second acquire did not proceed after first released")
	}

	if n := locks.size(); n != 0 {
		t.Fatalf("locks map size after all released = %d, want 0", n)
	}
}

func TestSandboxLifecycleLocks_RefcountUnderChurn(t *testing.T) {
	const ids = 100
	const perID = 50
	locks := NewSandboxLifecycleLocks()

	var wg sync.WaitGroup
	for i := 0; i < ids; i++ {
		id := "sbx-" + strconv.Itoa(i)
		for j := 0; j < perID; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				release := locks.Acquire(id)
				release()
			}()
		}
	}
	wg.Wait()

	if n := locks.size(); n != 0 {
		t.Fatalf("locks map leaked %d entries after churn, want 0", n)
	}
}

// size returns the current number of tracked lock entries. Test-only helper.
func (s *SandboxLifecycleLocks) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.locks)
}
