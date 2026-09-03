package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestActorRegistry_GetOrCreateSameID(t *testing.T) {
	r := NewActorRegistry()
	a := r.GetOrCreate("sbx-1")
	b := r.GetOrCreate("sbx-1")
	if a != b {
		t.Fatal("GetOrCreate returned different actors for the same id")
	}
	if n := r.size(); n != 1 {
		t.Fatalf("registry size = %d, want 1", n)
	}
}

func TestActor_EnqueueDoesNotWaitForFn(t *testing.T) {
	a := NewActorRegistry().GetOrCreate("sbx-1")

	started := make(chan struct{})
	unblock := make(chan struct{})
	if err := a.Enqueue(func() error {
		close(started)
		<-unblock
		return nil
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueued fn did not start")
	}
	close(unblock)
}

func TestActor_EnqueueSerializesDelete(t *testing.T) {
	a := NewActorRegistry().GetOrCreate("sbx-1")

	var order []string
	var mu sync.Mutex
	finishStarted := make(chan struct{})
	unblock := make(chan struct{})

	if err := a.Enqueue(func() error {
		mu.Lock()
		order = append(order, "finish")
		mu.Unlock()
		close(finishStarted)
		<-unblock
		return nil
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-finishStarted

	done := make(chan struct{})
	go func() {
		_ = a.Delete(context.Background(), func() error {
			mu.Lock()
			order = append(order, "delete")
			mu.Unlock()
			return nil
		})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Delete ran before enqueued finish completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(unblock)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not run after finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "finish" || order[1] != "delete" {
		t.Fatalf("order = %v, want [finish delete]", order)
	}
}

func TestActor_EnqueueAfterUnregister(t *testing.T) {
	r := NewActorRegistry()
	a := r.GetOrCreate("sbx-1")
	r.Unregister("sbx-1")
	if err := a.Enqueue(func() error { return nil }); err != errActorStopped {
		t.Fatalf("Enqueue after Unregister = %v, want errActorStopped", err)
	}
}

func TestActor_DoSerializesSameID(t *testing.T) {
	const goroutines = 50
	a := NewActorRegistry().GetOrCreate("sbx-1")

	var active, maxActive int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = a.Do(context.Background(), func() error {
				cur := atomic.AddInt32(&active, 1)
				for {
					prev := atomic.LoadInt32(&maxActive)
					if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&active, -1)
				return nil
			})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("max concurrent fns = %d, want 1", got)
	}
}

func TestActor_DoDoesNotBlockAcrossIDs(t *testing.T) {
	r := NewActorRegistry()
	a := r.GetOrCreate("sbx-a")
	b := r.GetOrCreate("sbx-b")

	started := make(chan struct{})
	unblock := make(chan struct{})
	go func() {
		_ = a.Do(context.Background(), func() error {
			close(started)
			<-unblock
			return nil
		})
	}()
	<-started

	done := make(chan struct{})
	go func() {
		_ = b.Do(context.Background(), func() error { return nil })
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Do on a different id blocked while another id held the inbox")
	}
	close(unblock)
}

func TestActor_AttachDoesNotUseInbox(t *testing.T) {
	a := NewActorRegistry().GetOrCreate("sbx-1")

	started := make(chan struct{})
	unblock := make(chan struct{})
	go func() {
		_ = a.Do(context.Background(), func() error {
			close(started)
			<-unblock
			return nil
		})
	}()
	<-started

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}

	done := make(chan struct{})
	go func() {
		a.Attach(cmd.Process)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach blocked on the inbox")
	}
	close(unblock)
}

func TestActor_AttachWaitReaps(t *testing.T) {
	a := NewActorRegistry().GetOrCreate("sbx-1")

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	a.Attach(cmd.Process)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			if os.IsNotExist(err) {
				return
			}
			t.Fatalf("stat /proc/%d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still in /proc after Wait should have reaped it", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestActorRegistry_UnregisterStopsActor(t *testing.T) {
	r := NewActorRegistry()
	a := r.GetOrCreate("sbx-1")
	r.Unregister("sbx-1")

	if n := r.size(); n != 0 {
		t.Fatalf("registry size after Unregister = %d, want 0", n)
	}

	err := a.Do(context.Background(), func() error { return nil })
	if err != errActorStopped {
		t.Fatalf("Do after Unregister = %v, want errActorStopped", err)
	}

	b := r.GetOrCreate("sbx-1")
	if a == b {
		t.Fatal("GetOrCreate after Unregister returned the stopped actor")
	}
}

func TestActor_SnapshotSerializesSameID(t *testing.T) {
	const goroutines = 50
	a := NewActorRegistry().GetOrCreate("sbx-1")

	var active, maxActive int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = a.Snapshot(context.Background(), func() error {
				cur := atomic.AddInt32(&active, 1)
				for {
					prev := atomic.LoadInt32(&maxActive)
					if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&active, -1)
				return nil
			})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("max concurrent Snapshots = %d, want 1", got)
	}
}

func TestActor_DoCancelAfterDequeueWaitsForFn(t *testing.T) {
	a := NewActorRegistry().GetOrCreate("sbx-1")
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Do(ctx, func() error {
			close(started)
			time.Sleep(50 * time.Millisecond)
			return errors.New("fn-done")
		})
	}()
	<-started
	cancel()

	select {
	case err := <-errCh:
		if err == nil || err.Error() != "fn-done" {
			t.Fatalf("Do = %v, want fn-done", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do did not return after fn finished")
	}
}

func TestActor_SnapshotHoldsLockAgainstTryAcquire(t *testing.T) {
	locks := NewSandboxLifecycleLocks()
	a := NewActorRegistry().GetOrCreate("sbx-1")

	held := make(chan struct{})
	unblock := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Snapshot(context.Background(), func() error {
			release := locks.Acquire("sbx-1")
			defer release()
			close(held)
			<-unblock
			return nil
		})
	}()
	<-held

	if rel := locks.TryAcquire("sbx-1"); rel != nil {
		rel()
		t.Fatal("TryAcquire succeeded while Snapshot held the lock")
	}
	close(unblock)
	if err := <-errCh; err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
}

func TestActor_DeleteIfSnapshottedNoopsAfterRestore(t *testing.T) {
	status := "snapshotted"
	locks := NewSandboxLifecycleLocks()
	a := NewActorRegistry().GetOrCreate("sbx-1")

	if err := a.Restore(context.Background(), func() error {
		release := locks.Acquire("sbx-1")
		defer release()
		if status != "snapshotted" {
			return errors.New("not snapshotted")
		}
		status = "running"
		return nil
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var deleted bool
	if err := a.Delete(context.Background(), func() error {
		release := locks.Acquire("sbx-1")
		defer release()
		if status != "snapshotted" {
			return nil
		}
		deleted = true
		status = "deleted"
		return nil
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted {
		t.Fatal("delete-if-snapshotted deleted after Restore")
	}
	if status != "running" {
		t.Fatalf("status = %s, want running", status)
	}
}

func TestActorAdmissionNilIsNoop(t *testing.T) {
	a := NewActorRegistry().GetOrCreate("sbx-1")
	if err := a.beforeBoot(context.Background(), 1, 512); err != nil {
		t.Fatal(err)
	}
	a.afterBoot(1, 512)
	a.afterBootFailed(1, 512)
	a.afterSnapshot(1, 512)
	a.afterDelete(true, 1, 512, 1024)
}

func TestActorAdmissionBootPath(t *testing.T) {
	rec := &recordingAdmission{}
	r := NewActorRegistry()
	r.SetAdmission(rec)
	a := r.GetOrCreate("sbx-1")

	if err := a.beforeBoot(context.Background(), 2, 4096); err != nil {
		t.Fatal(err)
	}
	a.afterBoot(2, 4096)
	if rec.beforeBoot != 1 || rec.afterBoot != 1 || rec.afterBootFailed != 0 {
		t.Fatalf("boot success calls: before=%d after=%d fail=%d", rec.beforeBoot, rec.afterBoot, rec.afterBootFailed)
	}

	rec.deny = true
	if err := a.beforeBoot(context.Background(), 2, 4096); !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("denied BeforeBoot = %v", err)
	}

	rec.deny = false
	if err := a.beforeBoot(context.Background(), 2, 4096); err != nil {
		t.Fatal(err)
	}
	a.afterBootFailed(2, 4096)
	if rec.afterBootFailed != 1 {
		t.Fatalf("afterBootFailed = %d, want 1", rec.afterBootFailed)
	}
}

func TestActorAdmissionSnapshotAndDelete(t *testing.T) {
	rec := &recordingAdmission{}
	r := NewActorRegistry()
	r.SetAdmission(rec)
	a := r.GetOrCreate("sbx-1")
	a.afterSnapshot(2, 4096)
	a.afterDelete(true, 2, 4096, 1024)
	a.afterDelete(false, 2, 4096, 1024)
	if rec.afterSnapshot != 1 || rec.afterDelete != 2 || rec.deleteRunning != 1 {
		t.Fatalf("snapshot=%d delete=%d running=%d", rec.afterSnapshot, rec.afterDelete, rec.deleteRunning)
	}
}

func TestRegistryAdmissionCreate(t *testing.T) {
	rec := &recordingAdmission{}
	r := NewActorRegistry()
	r.SetAdmission(rec)
	if err := r.beforeCreate(context.Background(), 1, 512, 1024); err != nil {
		t.Fatal(err)
	}
	r.afterCreate(1, 512, 1024)
	if rec.beforeCreate != 1 || rec.afterCreate != 1 {
		t.Fatalf("create success before=%d after=%d", rec.beforeCreate, rec.afterCreate)
	}
	r.afterCreateFailed(1, 512, 1024)
	if rec.afterCreateFailed != 1 {
		t.Fatalf("afterCreateFailed = %d", rec.afterCreateFailed)
	}
}

type recordingAdmission struct {
	deny              bool
	beforeCreate      int
	afterCreate       int
	afterCreateFailed int
	beforeBoot        int
	afterBoot         int
	afterBootFailed   int
	afterSnapshot     int
	afterDelete       int
	deleteRunning     int
}

func (r *recordingAdmission) BeforeCreate(context.Context, int, int, int) error {
	r.beforeCreate++
	if r.deny {
		return ErrAdmissionDenied
	}
	return nil
}
func (r *recordingAdmission) AfterCreate(int, int, int)       { r.afterCreate++ }
func (r *recordingAdmission) AfterCreateFailed(int, int, int) { r.afterCreateFailed++ }
func (r *recordingAdmission) BeforeBoot(context.Context, int, int) error {
	r.beforeBoot++
	if r.deny {
		return ErrAdmissionDenied
	}
	return nil
}
func (r *recordingAdmission) AfterBoot(int, int)       { r.afterBoot++ }
func (r *recordingAdmission) AfterBootFailed(int, int) { r.afterBootFailed++ }
func (r *recordingAdmission) AfterSnapshot(int, int)   { r.afterSnapshot++ }
func (r *recordingAdmission) AfterDelete(wasRunning bool, _, _, _ int) {
	r.afterDelete++
	if wasRunning {
		r.deleteRunning++
	}
}
