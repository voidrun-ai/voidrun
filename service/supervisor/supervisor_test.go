package supervisor

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

	"voidrun/model"
)

func TestSupervisorRegistry_GetOrCreateSameID(t *testing.T) {
	r := NewSupervisorRegistry()
	a := r.GetOrCreate("sbx-1")
	b := r.GetOrCreate("sbx-1")
	if a != b {
		t.Fatal("GetOrCreate returned different supervisors for the same id")
	}
	if n := r.size(); n != 1 {
		t.Fatalf("registry size = %d, want 1", n)
	}
}

func TestSupervisor_CoalesceDoesNotWaitForFn(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")

	started := make(chan struct{})
	unblock := make(chan struct{})
	if err := a.Coalesce(func() error {
		close(started)
		<-unblock
		return nil
	}); err != nil {
		t.Fatalf("Coalesce: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueued fn did not start")
	}
	close(unblock)
}

func TestSupervisor_CoalesceSerializesDelete(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")

	var order []string
	var mu sync.Mutex
	finishStarted := make(chan struct{})
	unblock := make(chan struct{})

	if err := a.Coalesce(func() error {
		mu.Lock()
		order = append(order, "finish")
		mu.Unlock()
		close(finishStarted)
		<-unblock
		return nil
	}); err != nil {
		t.Fatalf("Coalesce: %v", err)
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

func TestSupervisor_CoalesceAfterUnregister(t *testing.T) {
	r := NewSupervisorRegistry()
	a := r.GetOrCreate("sbx-1")
	r.Unregister("sbx-1")
	if err := a.Coalesce(func() error { return nil }); err != ErrSupervisorStopped {
		t.Fatalf("Coalesce after Unregister = %v, want ErrSupervisorStopped", err)
	}
}

func TestSupervisor_CoalesceKeepsLatestOnly(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
	t.Cleanup(func() { a.shutdown() })

	var mu sync.Mutex
	var runs []int
	hold := make(chan struct{})
	started := make(chan struct{})

	if err := a.Coalesce(func() error {
		mu.Lock()
		runs = append(runs, -1)
		mu.Unlock()
		close(started)
		<-hold
		return nil
	}); err != nil {
		t.Fatalf("Coalesce: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced fn did not start")
	}

	follow := make(chan struct{})
	for i := 0; i < 10; i++ {
		n := i
		if err := a.Coalesce(func() error {
			mu.Lock()
			runs = append(runs, n)
			mu.Unlock()
			if n == 9 {
				close(follow)
			}
			return nil
		}); err != nil {
			t.Fatalf("Coalesce %d: %v", n, err)
		}
	}
	close(hold)
	select {
	case <-follow:
	case <-time.After(2 * time.Second):
		t.Fatal("latest coalesced fn did not run")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 2 || runs[0] != -1 || runs[1] != 9 {
		t.Fatalf("runs = %v, want [-1 9]", runs)
	}
}

func TestSupervisor_DoSerializesSameID(t *testing.T) {
	const goroutines = 50
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")

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

func TestSupervisor_DoDoesNotBlockAcrossIDs(t *testing.T) {
	r := NewSupervisorRegistry()
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

func TestSupervisor_AttachDoesNotUseInbox(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")

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
		if err := a.Attach(cmd.Process); err != nil {
			t.Errorf("Attach: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach blocked on the inbox")
	}
	close(unblock)
}

func TestSupervisor_WatchPIDExit(t *testing.T) {
	r := NewSupervisorRegistry()
	var n atomic.Int32
	r.SetOnProcessExit(func(string) { n.Add(1) })
	a := r.GetOrCreate("sbx-watch")
	t.Cleanup(func() { r.Unregister("sbx-watch") })

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	if a.SupervisesProcess() {
		t.Fatal("SupervisesProcess before WatchPID")
	}
	a.WatchPID(cmd.Process.Pid)
	if !a.SupervisesProcess() {
		t.Fatal("WatchPID did not set SupervisesProcess")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for n.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("onProcessExit not called after pidfd exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSupervisor_WatchPIDNoOpWhenAttached(t *testing.T) {
	r := NewSupervisorRegistry()
	a := r.GetOrCreate("sbx-1")
	t.Cleanup(func() { r.Unregister("sbx-1") })

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	if err := a.Attach(cmd.Process); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	a.WatchPID(cmd.Process.Pid)
	if !a.SupervisesProcess() {
		t.Fatal("Attach should keep SupervisesProcess")
	}
}

func TestSupervisorRegistry_SupervisesProcessRequiresWatch(t *testing.T) {
	r := NewSupervisorRegistry()
	if r.SupervisesProcess("missing") {
		t.Fatal("missing id must not supervise")
	}
	_ = r.GetOrCreate("sbx-1")
	t.Cleanup(func() { r.Unregister("sbx-1") })
	if r.Has("sbx-1") && r.SupervisesProcess("sbx-1") {
		t.Fatal("Has without Attach/WatchPID must not skip health")
	}
}

func TestSupervisor_AttachWaitReaps(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	if err := a.Attach(cmd.Process); err != nil {
		t.Fatalf("Attach: %v", err)
	}

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

func TestSupervisorRegistry_UnregisterStopsSupervisor(t *testing.T) {
	r := NewSupervisorRegistry()
	a := r.GetOrCreate("sbx-1")
	r.Unregister("sbx-1")

	if n := r.size(); n != 0 {
		t.Fatalf("registry size after Unregister = %d, want 0", n)
	}

	err := a.Do(context.Background(), func() error { return nil })
	if err != ErrSupervisorStopped {
		t.Fatalf("Do after Unregister = %v, want ErrSupervisorStopped", err)
	}

	b := r.GetOrCreate("sbx-1")
	if a == b {
		t.Fatal("GetOrCreate after Unregister returned the stopped supervisor")
	}
}

func TestSupervisor_SnapshotSerializesSameID(t *testing.T) {
	const goroutines = 50
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")

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

func TestSupervisor_DoCancelAfterDequeueWaitsForFn(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
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

func TestSupervisor_DeleteIfSnapshottedNoopsAfterRestore(t *testing.T) {
	status := "snapshotted"
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")

	if err := a.Restore(context.Background(), func() error {
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

func TestSupervisorAdmissionNilIsNoop(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
	if err := a.BeforeBoot(context.Background(), 1, 512); err != nil {
		t.Fatal(err)
	}
	a.AfterBoot(1, 512)
	a.AfterBootFailed(1, 512)
	a.AfterSnapshot(1, 512)
	a.AfterDelete(true, 1, 512, 1024)
}

func TestSupervisorAdmissionBootPath(t *testing.T) {
	rec := &recordingAdmission{}
	r := NewSupervisorRegistry()
	r.SetAdmission(rec)
	a := r.GetOrCreate("sbx-1")

	if err := a.BeforeBoot(context.Background(), 2, 4096); err != nil {
		t.Fatal(err)
	}
	a.AfterBoot(2, 4096)
	if rec.beforeBoot != 1 || rec.afterBoot != 1 || rec.afterBootFailed != 0 {
		t.Fatalf("boot success calls: before=%d after=%d fail=%d", rec.beforeBoot, rec.afterBoot, rec.afterBootFailed)
	}

	rec.deny = true
	if err := a.BeforeBoot(context.Background(), 2, 4096); !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("denied BeforeBoot = %v", err)
	}

	rec.deny = false
	if err := a.BeforeBoot(context.Background(), 2, 4096); err != nil {
		t.Fatal(err)
	}
	a.AfterBootFailed(2, 4096)
	if rec.afterBootFailed != 1 {
		t.Fatalf("afterBootFailed = %d, want 1", rec.afterBootFailed)
	}
}

func TestSupervisorAdmissionSnapshotAndDelete(t *testing.T) {
	rec := &recordingAdmission{}
	r := NewSupervisorRegistry()
	r.SetAdmission(rec)
	a := r.GetOrCreate("sbx-1")
	a.AfterSnapshot(2, 4096)
	a.AfterDelete(true, 2, 4096, 1024)
	a.AfterDelete(false, 2, 4096, 1024)
	if rec.afterSnapshot != 1 || rec.afterDelete != 2 || rec.deleteRunning != 1 {
		t.Fatalf("snapshot=%d delete=%d running=%d", rec.afterSnapshot, rec.afterDelete, rec.deleteRunning)
	}
}

func TestRegistryAdmissionCreate(t *testing.T) {
	rec := &recordingAdmission{}
	r := NewSupervisorRegistry()
	r.SetAdmission(rec)
	if err := r.BeforeCreate(context.Background(), 1, 512, 1024); err != nil {
		t.Fatal(err)
	}
	r.AfterCreate(1, 512, 1024)
	if rec.beforeCreate != 1 || rec.afterCreate != 1 {
		t.Fatalf("create success before=%d after=%d", rec.beforeCreate, rec.afterCreate)
	}
	r.AfterCreateFailed(1, 512, 1024)
	if rec.afterCreateFailed != 1 {
		t.Fatalf("afterCreateFailed = %d", rec.afterCreateFailed)
	}
}

func TestSupervisorRegistry_Has(t *testing.T) {
	r := NewSupervisorRegistry()
	if r.Has("sbx-1") {
		t.Fatal("Has before GetOrCreate")
	}
	r.GetOrCreate("sbx-1")
	if !r.Has("sbx-1") {
		t.Fatal("Has after GetOrCreate")
	}
	if r.Has("sbx-other") {
		t.Fatal("Has for a different id")
	}
	r.Unregister("sbx-1")
	if r.Has("sbx-1") {
		t.Fatal("Has after Unregister")
	}
}

func TestSupervisor_OnWaitNotifiesProcessExit(t *testing.T) {
	r := NewSupervisorRegistry()
	got := make(chan string, 1)
	r.SetOnProcessExit(func(id string) { got <- id })
	a := r.GetOrCreate("sbx-1")

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}
	if err := a.Attach(cmd.Process); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	select {
	case id := <-got:
		if id != "sbx-1" {
			t.Fatalf("id = %s, want sbx-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onProcessExit not called after Wait")
	}
}

func TestSupervisor_OnWaitStaleGenSkipped(t *testing.T) {
	r := NewSupervisorRegistry()
	var n int32
	r.SetOnProcessExit(func(string) { atomic.AddInt32(&n, 1) })
	a := r.GetOrCreate("sbx-1")

	cmd1 := exec.Command("sleep", "60")
	if err := cmd1.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	if err := a.Attach(cmd1.Process); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	cmd2 := exec.Command("true")
	if err := cmd2.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}
	if err := a.Attach(cmd2.Process); err != nil {
		t.Fatalf("second Attach: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&n) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("onProcessExit not called for current gen")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := cmd1.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("onProcessExit calls = %d, want 1 (stale gen skipped)", got)
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
func (r *recordingAdmission) BeforeWake(context.Context, *model.Sandbox) error { return nil }
func (r *recordingAdmission) AfterWake(context.Context, *model.Sandbox)        {}

func TestSupervisor_EmitLifecycleOrder(t *testing.T) {
	r := NewSupervisorRegistry()
	names := make(chan string, 2)
	r.AddLifecycleListener(func(ev model.LifecycleEvent) {
		names <- ev.EventName()
	})
	a := r.GetOrCreate("sbx-1")
	a.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpSleep, Phase: model.PhaseStarting, FromStatus: "running", ToStatus: "snapshotted"})
	a.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpSleep, Phase: model.PhaseCommitted, FromStatus: "running", ToStatus: "snapshotted"})
	got := []string{recvLife(t, names), recvLife(t, names)}
	if got[0] != "sleep-starting" || got[1] != "sleep-committed" {
		t.Fatalf("listener names = %v", got)
	}
}

func TestSupervisor_EmitLifecycleCommittedNotifies(t *testing.T) {
	r := NewSupervisorRegistry()
	called := make(chan struct{})
	r.AddLifecycleListener(func(model.LifecycleEvent) { close(called) })
	r.GetOrCreate("sbx-1").EmitLifecycle(context.Background(), model.LifecycleEvent{
		Op: model.OpSleep, Phase: model.PhaseCommitted,
	})
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("committed listener must run")
	}
}

func TestSupervisor_LifecycleDeliveryKeepsOrder(t *testing.T) {
	r := NewSupervisorRegistry()
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var order []string
	r.AddLifecycleListener(func(ev model.LifecycleEvent) {
		if ev.Op == model.OpWake && ev.Phase == model.PhaseCommitted {
			mu.Lock()
			order = append(order, ev.EventName())
			mu.Unlock()
			once.Do(func() { close(started) })
			<-release
			return
		}
		mu.Lock()
		order = append(order, ev.EventName())
		mu.Unlock()
	})
	a := r.GetOrCreate("sbx-1")

	emitDone := make(chan struct{})
	go func() {
		a.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpWake, Phase: model.PhaseCommitted})
		close(emitDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("committed listener did not start")
	}
	select {
	case <-emitDone:
	case <-time.After(time.Second):
		t.Fatal("committed emit blocked on the listener")
	}

	secondDone := make(chan struct{})
	go func() {
		a.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpSleep, Phase: model.PhaseCommitted})
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second committed emit blocked on an earlier listener")
	}

	startDone := make(chan struct{})
	go func() {
		a.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpSleep, Phase: model.PhaseStarting})
		close(startDone)
	}()
	select {
	case <-startDone:
		t.Fatal("starting returned while an earlier listener was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-startDone:
	case <-time.After(time.Second):
		t.Fatal("starting did not finish after earlier listeners")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"wake-committed", "sleep-committed", "sleep-starting"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v", order)
		}
	}
}

func TestSupervisor_LifecycleLanesDoNotBlockOtherSandboxes(t *testing.T) {
	r := NewSupervisorRegistry()
	release := make(chan struct{})
	started := make(chan struct{})
	r.AddLifecycleListener(func(ev model.LifecycleEvent) {
		if ev.SandboxID == "sbx-a" {
			close(started)
			<-release
		}
	})
	a := r.GetOrCreate("sbx-a")
	b := r.GetOrCreate("sbx-b")
	go a.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpWake, Phase: model.PhaseCommitted})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("committed listener did not start")
	}
	done := make(chan struct{})
	go func() {
		b.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpSleep, Phase: model.PhaseStarting})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("other sandbox starting waited on this lane")
	}
	close(release)
}

func TestSupervisor_WaitErrorStillNotifiesExit(t *testing.T) {
	r := NewSupervisorRegistry()
	called := make(chan struct{}, 1)
	r.SetOnProcessExit(func(string) { called <- struct{}{} })
	a := r.GetOrCreate("sbx-1")
	a.mu.Lock()
	a.waitGen = 1
	a.mu.Unlock()
	a.onWait(waitResult{gen: 1, err: errors.New("wait failed")})
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("onProcessExit not called after Wait error (row would strand as running)")
	}
}

func TestSupervisorRegistry_EmitDoesNotResurrectStoppedSupervisor(t *testing.T) {
	r := NewSupervisorRegistry()
	got := make(chan string, 1)
	r.AddLifecycleListener(func(ev model.LifecycleEvent) { got <- ev.EventName() })
	r.GetOrCreate("sbx-1")
	r.Unregister("sbx-1")

	r.Emit(context.Background(), "sbx-1", model.LifecycleEvent{Op: model.OpCreate, Phase: model.PhaseFailed})

	if r.Has("sbx-1") {
		t.Fatal("Emit resurrected an unregistered supervisor")
	}
	select {
	case name := <-got:
		if name != "create-failed" {
			t.Fatalf("listener = %q, want create-failed", name)
		}
	case <-time.After(time.Second):
		t.Fatal("listener was not called")
	}
}

func TestLifecycleLaneDroppedWhenIdle(t *testing.T) {
	r := NewSupervisorRegistry()
	done := make(chan struct{})
	r.AddLifecycleListener(func(model.LifecycleEvent) { close(done) })
	r.Emit(context.Background(), "sbx-1", model.LifecycleEvent{Op: model.OpCreate, Phase: model.PhaseCommitted})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener was not called")
	}
	deadline := time.Now().Add(time.Second)
	for r.laneCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("life lanes = %d, want 0", r.laneCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLifecycleGenerationSurvivesIdleLane(t *testing.T) {
	r := NewSupervisorRegistry()
	var gens []uint64
	done := make(chan struct{}, 1)
	r.AddLifecycleListener(func(ev model.LifecycleEvent) {
		gens = append(gens, ev.Generation)
		done <- struct{}{}
	})
	ev := model.LifecycleEvent{Op: model.OpSleep, Phase: model.PhaseCommitted}
	r.Emit(context.Background(), "sbx-1", ev)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("first listener did not run")
	}
	deadline := time.Now().Add(time.Second)
	for r.laneCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("lane was not dropped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Emit(context.Background(), "sbx-1", ev)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second listener did not run")
	}
	if len(gens) != 2 || gens[0] != 1 || gens[1] != 2 {
		t.Fatalf("generations = %v, want [1 2]", gens)
	}
}

func TestLifecycleGenerationsIncreaseAcrossEmitters(t *testing.T) {
	r := NewSupervisorRegistry()
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var gens []uint64
	var n atomic.Int32
	r.AddLifecycleListener(func(ev model.LifecycleEvent) {
		once.Do(func() {
			close(entered)
			<-release
		})
		mu.Lock()
		gens = append(gens, ev.Generation)
		mu.Unlock()
		n.Add(1)
	})
	a := r.GetOrCreate("sbx-1")
	const total = 40
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.EmitLifecycle(context.Background(), model.LifecycleEvent{Op: model.OpSleep, Phase: model.PhaseCommitted})
		}()
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not start")
	}
	wg.Wait()
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < total {
		if time.Now().After(deadline) {
			t.Fatalf("listeners = %d, want %d", n.Load(), total)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gens) != total {
		t.Fatalf("gens = %d, want %d", len(gens), total)
	}
	for i := 1; i < len(gens); i++ {
		if gens[i] <= gens[i-1] {
			t.Fatalf("generations not increasing: %v", gens)
		}
	}
}

func TestSupervisor_AttachAfterStopReturnsWithoutWaiting(t *testing.T) {
	r := NewSupervisorRegistry()
	a := r.GetOrCreate("sbx-1")
	r.Unregister("sbx-1")

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	done := make(chan error, 1)
	go func() { done <- a.Attach(cmd.Process) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSupervisorStopped) {
			t.Fatalf("Attach = %v, want ErrSupervisorStopped", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Attach waited after the supervisor stopped")
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(cmd.Process.Pid)); err != nil {
		t.Fatalf("caller no longer owns the process: %v", err)
	}
}

func TestSupervisor_UnregisterKillsAttachedProcess(t *testing.T) {
	r := NewSupervisorRegistry()
	a := r.GetOrCreate("sbx-1")
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	if err := a.Attach(cmd.Process); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	r.Unregister("sbx-1")

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat("/proc/" + strconv.Itoa(pid))
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatalf("stat /proc/%d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive after Unregister", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSupervisor_IdleTimerFires(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
	done := make(chan struct{})
	a.SetIdleAfter(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle timer did not fire")
	}
}

func TestSupervisor_IdleTimerResetCancelsPrior(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
	done := make(chan struct{})
	a.SetIdleAfter(time.Hour, func() { t.Error("prior idle timer fired") })
	a.SetIdleAfter(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reset idle timer did not fire")
	}
}

func TestSupervisor_UnregisterStopsIdleTimer(t *testing.T) {
	r := NewSupervisorRegistry()
	a := r.GetOrCreate("sbx-1")
	fired := make(chan struct{})
	a.SetIdleAfter(25*time.Millisecond, func() { close(fired) })
	r.Unregister("sbx-1")
	select {
	case <-fired:
		t.Fatal("idle timer fired after Unregister")
	case <-time.After(80 * time.Millisecond):
	}
}

func recvLife(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case name := <-ch:
		return name
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for supervisor event")
	}
	return ""
}

func TestSupervisor_CommandPanicDoesNotKillLoop(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
	err := a.Do(context.Background(), func() error { panic("boom") })
	if !errors.Is(err, errSupervisorCommandPanic) {
		t.Fatalf("Do = %v, want command panic", err)
	}
	if err := a.Do(context.Background(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisor_TimerPanicStaysInProcess(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
	a.SetIdleAfter(time.Millisecond, func() { panic("timer") })
	time.Sleep(30 * time.Millisecond)
	if err := a.Do(context.Background(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisor_DeleteRunsAheadOfQueuedCommand(t *testing.T) {
	a := NewSupervisorRegistry().GetOrCreate("sbx-1")
	hold := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = a.Do(context.Background(), func() error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started

	order := make(chan string, 2)
	go func() {
		_ = a.Snapshot(context.Background(), func() error {
			order <- "snapshot"
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		_ = a.Delete(context.Background(), func() error {
			order <- "delete"
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	close(hold)

	if got := recvLife(t, order); got != "delete" {
		t.Fatalf("first finished command = %s, want delete", got)
	}
}
