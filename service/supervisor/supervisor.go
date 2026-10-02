package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"voidrun/model"
	"voidrun/runtime"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	ErrSupervisorStopped      = errors.New("sandbox supervisor stopped")
	errSupervisorCommandPanic = errors.New("sandbox supervisor command panicked")
)

type cmdKind string

const (
	cmdSnapshot cmdKind = "snapshot"
	cmdDelete   cmdKind = "delete"
	cmdRestore  cmdKind = "restore"
	cmdStart    cmdKind = "start"
	cmdCoalesce cmdKind = "coalesce"
)

type envelope struct {
	kind cmdKind
	fn   func() error
	done chan error
	at   time.Time
}

type waitResult struct {
	gen   uint64
	state *os.ProcessState
	err   error
}

// Supervisor is one goroutine + inbox per sandbox. Commands run one at a time.
type Supervisor struct {
	id       string
	reg      *SupervisorRegistry
	inbox    chan envelope
	urgent   chan envelope
	waitCh   chan waitResult
	stop     chan struct{}
	stopOnce sync.Once

	mu      sync.Mutex
	proc    *os.Process
	waitGen uint64

	coalesceFn    func() error
	coalesceBusy  bool
	coalesceAgain bool

	orgID     primitive.ObjectID
	autoSleep bool
	idleTimer *time.Timer
	idleGen   uint64
	watching  bool

	lastActivity time.Time
}

func newSupervisor(id string, r *SupervisorRegistry) *Supervisor {
	return &Supervisor{
		id:     id,
		reg:    r,
		inbox:  make(chan envelope),
		urgent: make(chan envelope),
		waitCh: make(chan waitResult, 1),
		stop:   make(chan struct{}),
	}
}

// Do enqueues fn and waits until it finishes. ctx cancels the enqueue wait
// only; once dequeued, fn runs to completion.
func (a *Supervisor) Do(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, "", fn)
}

func (a *Supervisor) Snapshot(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdSnapshot, fn)
}

func (a *Supervisor) Delete(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdDelete, fn)
}

func (a *Supervisor) Restore(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdRestore, fn)
}

func (a *Supervisor) Start(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdStart, fn)
}

// NoteActivity reports whether id has not been touched within gap.
func (a *Supervisor) NoteActivity(gap time.Duration) bool {
	if a == nil {
		return true
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.lastActivity.IsZero() && now.Sub(a.lastActivity) < gap {
		return false
	}
	a.lastActivity = now
	return true
}

// Coalesce runs fn on the supervisor without waiting. A later call before the run
// finishes replaces fn, so only the latest update runs once more.
// Must not be called from an inbox command (deadlock).
func (a *Supervisor) Coalesce(fn func() error) error {
	if a == nil || fn == nil {
		return nil
	}
	a.mu.Lock()
	select {
	case <-a.stop:
		a.mu.Unlock()
		return ErrSupervisorStopped
	default:
	}
	a.coalesceFn = fn
	if a.coalesceBusy {
		a.coalesceAgain = true
		a.mu.Unlock()
		return nil
	}
	a.coalesceBusy = true
	a.mu.Unlock()
	go a.coalesceLoop()
	return nil
}

func (a *Supervisor) coalesceLoop() {
	for {
		a.mu.Lock()
		fn := a.coalesceFn
		a.coalesceAgain = false
		a.mu.Unlock()
		if err := a.doKind(context.Background(), cmdCoalesce, fn); err != nil {
			if errors.Is(err, ErrSupervisorStopped) {
				a.mu.Lock()
				a.coalesceBusy = false
				a.mu.Unlock()
				return
			}
			log.Printf("[supervisor] %s coalesce: %v", a.id, err)
		}
		a.mu.Lock()
		if !a.coalesceAgain {
			a.coalesceBusy = false
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
	}
}

func (a *Supervisor) doKind(ctx context.Context, kind cmdKind, fn func() error) error {
	if fn == nil {
		return nil
	}
	done := make(chan error, 1)
	env := envelope{kind: kind, fn: fn, done: done, at: time.Now()}
	ch := a.inbox
	if kind == cmdDelete {
		ch = a.urgent
	}
	select {
	case ch <- env:
	case <-ctx.Done():
		return ctx.Err()
	case <-a.stop:
		return ErrSupervisorStopped
	}
	return <-done
}

func (a *Supervisor) BeforeBoot(ctx context.Context, cpu, mem int) error {
	if a.reg == nil {
		return nil
	}
	adm := a.reg.adm()
	if adm == nil {
		return nil
	}
	return adm.BeforeBoot(ctx, cpu, mem)
}

func (a *Supervisor) AfterBoot(cpu, mem int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterBoot(cpu, mem)
	}
}

func (a *Supervisor) AfterBootFailed(cpu, mem int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterBootFailed(cpu, mem)
	}
}

func (a *Supervisor) AfterSnapshot(cpu, mem int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterSnapshot(cpu, mem)
	}
}

func (a *Supervisor) AfterDelete(wasRunning bool, cpu, mem, diskMB int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterDelete(wasRunning, cpu, mem, diskMB)
	}
}

func (a *Supervisor) BeforeWake(ctx context.Context, sb *model.Sandbox) error {
	if a == nil || a.reg == nil || sb == nil {
		return nil
	}
	adm := a.reg.adm()
	if adm == nil {
		return nil
	}
	return adm.BeforeWake(ctx, sb)
}

func (a *Supervisor) AfterWake(ctx context.Context, sb *model.Sandbox) {
	if a == nil || a.reg == nil || sb == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterWake(ctx, sb)
	}
}

// SupervisesProcess is true when this supervisor is waiting on a child or watching
// an adopted pid. Health must skip only then — Has(id) is not enough.
func (a *Supervisor) SupervisesProcess() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proc != nil || a.watching
}

// WatchPID watches a process this supervisor did not spawn (pidfd). No-op if already
// supervising. Must not send on the inbox.
func (a *Supervisor) WatchPID(pid int) {
	if a == nil || pid <= 0 {
		return
	}
	select {
	case <-a.stop:
		return
	default:
	}

	a.mu.Lock()
	if a.proc != nil || a.watching {
		a.mu.Unlock()
		return
	}
	a.waitGen++
	gen := a.waitGen
	a.watching = true
	a.mu.Unlock()

	go func() {
		err := runtime.WatchPID(pid, a.stop)
		a.mu.Lock()
		if a.waitGen == gen {
			a.watching = false
		}
		a.mu.Unlock()
		if errors.Is(err, runtime.ErrWatchStopped) {
			return
		}
		if err != nil {
			log.Printf("[supervisor] %s pid watch: %v", a.id, err)
			return
		}
		select {
		case a.waitCh <- waitResult{gen: gen}:
		case <-a.stop:
		}
	}()
}

// Attach takes ownership of proc and watches it with pidfd. After exit, Wait
// reaps the child. On error the caller still owns proc. A later Attach replaces
// the previous process; the previous exit is ignored. Safe to call from inside Do.
func (a *Supervisor) Attach(proc *os.Process) error {
	if proc == nil {
		return errors.New("nil process")
	}
	a.mu.Lock()
	select {
	case <-a.stop:
		a.mu.Unlock()
		return ErrSupervisorStopped
	default:
	}
	a.waitGen++
	gen := a.waitGen
	a.proc = proc
	a.watching = false
	a.mu.Unlock()

	go func() {
		err := runtime.WatchPID(proc.Pid, a.stop)
		state, waitErr := proc.Wait()
		if errors.Is(err, runtime.ErrWatchStopped) {
			return
		}
		if err == nil {
			err = waitErr
		}
		select {
		case a.waitCh <- waitResult{gen: gen, state: state, err: err}:
		case <-a.stop:
		}
	}()
	return nil
}

func (a *Supervisor) shutdown() {
	a.stopOnce.Do(func() { close(a.stop) })
	a.mu.Lock()
	proc := a.proc
	a.mu.Unlock()
	if proc != nil {
		_ = proc.Kill()
	}
}

func (a *Supervisor) SetAutoLifeMeta(orgID primitive.ObjectID, autoSleep bool) {
	a.mu.Lock()
	a.orgID = orgID
	a.autoSleep = autoSleep
	a.mu.Unlock()
}

func (a *Supervisor) AutoLifeMeta() (primitive.ObjectID, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.orgID, a.autoSleep
}

func (a *Supervisor) loop() {
	for {
		select {
		case <-a.stop:
			return
		case env := <-a.urgent:
			a.runCmd(env)
			continue
		default:
		}
		select {
		case <-a.stop:
			return
		case env := <-a.urgent:
			a.runCmd(env)
		case env := <-a.inbox:
			a.runCmd(env)
		case wr := <-a.waitCh:
			a.onWait(wr)
		}
	}
}

func (a *Supervisor) runCmd(env envelope) {
	if a.reg != nil && !env.at.IsZero() {
		kind := string(env.kind)
		if kind == "" {
			kind = "command"
		}
		a.reg.noteQueueWait(kind, time.Since(env.at))
	}
	var err error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[supervisor] %s command panic: %v", a.id, rec)
				err = fmt.Errorf("%w: %v", errSupervisorCommandPanic, rec)
			}
		}()
		err = env.fn()
	}()
	env.done <- err
}

func (a *Supervisor) onWait(wr waitResult) {
	a.mu.Lock()
	match := wr.gen == a.waitGen
	if match {
		a.proc = nil
		a.watching = false
	}
	a.mu.Unlock()
	if !match {
		return
	}

	if wr.err != nil {
		log.Printf("[supervisor] %s wait: %v", a.id, wr.err)
	} else if wr.state != nil {
		log.Printf("[supervisor] %s exited: %s", a.id, wr.state)
	}
	// Notify even on Wait error: in practice it means the process was already
	// reaped (ECHILD). Skipping it would strand the row as "running" because
	// the sweeper skips registered supervisors; the CAS guard bounds the damage.
	if a.reg != nil {
		id := a.id
		reg := a.reg
		go reg.notifyProcessExit(id)
	}
}

// EmitLifecycle fans the event out to listeners. Safe from inside an inbox fn.
func (a *Supervisor) EmitLifecycle(ctx context.Context, ev model.LifecycleEvent) {
	if a.reg == nil {
		return
	}
	ev.SandboxID = a.id
	a.reg.publishLifecycle(ctx, ev)
}
