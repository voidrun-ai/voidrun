package service

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
)

var errActorStopped = errors.New("sandbox actor stopped")

type cmdKind string

const (
	cmdSnapshot cmdKind = "snapshot"
	cmdDelete   cmdKind = "delete"
	cmdRestore  cmdKind = "restore"
	cmdStart    cmdKind = "start"
	cmdFinish   cmdKind = "finish"
)

type envelope struct {
	kind cmdKind
	fn   func() error
	done chan error
}

type waitResult struct {
	gen   uint64
	state *os.ProcessState
	err   error
}

// ErrAdmissionDenied is returned when an Admission plugin refuses a command.
var ErrAdmissionDenied = errors.New("admission denied")

// Admission is an optional plugin called at lifecycle check-in points.
type Admission interface {
	BeforeCreate(ctx context.Context, cpu, mem, diskMB int) error
	AfterCreate(cpu, mem, diskMB int)
	AfterCreateFailed(cpu, mem, diskMB int)
	BeforeBoot(ctx context.Context, cpu, mem int) error
	AfterBoot(cpu, mem int)
	AfterBootFailed(cpu, mem int)
	AfterSnapshot(cpu, mem int)
	AfterDelete(wasRunning bool, cpu, mem, diskMB int)
}

// ActorRegistry maps sandbox ID to a per-sandbox actor.
type ActorRegistry struct {
	mu        sync.Mutex
	actors    map[string]*Actor
	admission Admission
}

func NewActorRegistry() *ActorRegistry {
	return &ActorRegistry{actors: make(map[string]*Actor)}
}

// SetAdmission installs the lifecycle plugin. Nil clears it.
func (r *ActorRegistry) SetAdmission(a Admission) {
	r.mu.Lock()
	r.admission = a
	r.mu.Unlock()
}

func (r *ActorRegistry) adm() Admission {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.admission
}

// GetOrCreate returns the actor for id, starting its loop on first use.
func (r *ActorRegistry) GetOrCreate(id string) *Actor {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.actors[id]; ok {
		return a
	}
	a := newActor(id, r)
	r.actors[id] = a
	go a.loop()
	return a
}

// Unregister stops the actor for id and drops it from the map. No-op if absent.
func (r *ActorRegistry) Unregister(id string) {
	r.mu.Lock()
	a, ok := r.actors[id]
	if ok {
		delete(r.actors, id)
	}
	r.mu.Unlock()
	if ok {
		a.shutdown()
	}
}

func (r *ActorRegistry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.actors)
}

// Actor is one goroutine + inbox per sandbox. Commands run one at a time.
type Actor struct {
	id       string
	reg      *ActorRegistry
	inbox    chan envelope
	waitCh   chan waitResult
	stop     chan struct{}
	stopOnce sync.Once

	mu      sync.Mutex
	proc    *os.Process
	waitGen uint64
}

func newActor(id string, r *ActorRegistry) *Actor {
	return &Actor{
		id:     id,
		reg:    r,
		inbox:  make(chan envelope),
		waitCh: make(chan waitResult, 1),
		stop:   make(chan struct{}),
	}
}

// Do enqueues fn and waits until it finishes. ctx cancels the enqueue wait
// only; once dequeued, fn runs to completion.
func (a *Actor) Do(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, "", fn)
}

func (a *Actor) Snapshot(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdSnapshot, fn)
}

func (a *Actor) Delete(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdDelete, fn)
}

func (a *Actor) Restore(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdRestore, fn)
}

func (a *Actor) Start(ctx context.Context, fn func() error) error {
	return a.doKind(ctx, cmdStart, fn)
}

// Enqueue queues fn and returns once the inbox has accepted it. It does not
// wait for fn to finish. done is buffered so the loop can complete without a waiter.
func (a *Actor) Enqueue(fn func() error) error {
	if fn == nil {
		return nil
	}
	done := make(chan error, 1)
	select {
	case a.inbox <- envelope{kind: cmdFinish, fn: fn, done: done}:
		return nil
	case <-a.stop:
		return errActorStopped
	}
}

func (a *Actor) doKind(ctx context.Context, kind cmdKind, fn func() error) error {
	if fn == nil {
		return nil
	}
	done := make(chan error, 1)
	select {
	case a.inbox <- envelope{kind: kind, fn: fn, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-a.stop:
		return errActorStopped
	}
	return <-done
}

func (r *ActorRegistry) beforeCreate(ctx context.Context, cpu, mem, diskMB int) error {
	a := r.adm()
	if a == nil {
		return nil
	}
	return a.BeforeCreate(ctx, cpu, mem, diskMB)
}

func (r *ActorRegistry) afterCreate(cpu, mem, diskMB int) {
	if a := r.adm(); a != nil {
		a.AfterCreate(cpu, mem, diskMB)
	}
}

func (r *ActorRegistry) afterCreateFailed(cpu, mem, diskMB int) {
	if a := r.adm(); a != nil {
		a.AfterCreateFailed(cpu, mem, diskMB)
	}
}

func (a *Actor) beforeBoot(ctx context.Context, cpu, mem int) error {
	if a.reg == nil {
		return nil
	}
	adm := a.reg.adm()
	if adm == nil {
		return nil
	}
	return adm.BeforeBoot(ctx, cpu, mem)
}

func (a *Actor) afterBoot(cpu, mem int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterBoot(cpu, mem)
	}
}

func (a *Actor) afterBootFailed(cpu, mem int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterBootFailed(cpu, mem)
	}
}

func (a *Actor) afterSnapshot(cpu, mem int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterSnapshot(cpu, mem)
	}
}

func (a *Actor) afterDelete(wasRunning bool, cpu, mem, diskMB int) {
	if a.reg == nil {
		return
	}
	if adm := a.reg.adm(); adm != nil {
		adm.AfterDelete(wasRunning, cpu, mem, diskMB)
	}
}

// Attach records proc and starts Wait. It must not send on the inbox so it is
// safe to call from inside Do
func (a *Actor) Attach(proc *os.Process) {
	if proc == nil {
		return
	}
	select {
	case <-a.stop:
		_, _ = proc.Wait()
		return
	default:
	}

	a.mu.Lock()
	a.waitGen++
	gen := a.waitGen
	a.proc = proc
	a.mu.Unlock()

	go func() {
		state, err := proc.Wait()
		select {
		case a.waitCh <- waitResult{gen: gen, state: state, err: err}:
		case <-a.stop:
		}
	}()
}

func (a *Actor) shutdown() {
	a.stopOnce.Do(func() { close(a.stop) })
}

func (a *Actor) loop() {
	for {
		select {
		case env := <-a.inbox:
			env.done <- env.fn()
		case wr := <-a.waitCh:
			a.onWait(wr)
		case <-a.stop:
			return
		}
	}
}

func (a *Actor) onWait(wr waitResult) {
	a.mu.Lock()
	match := wr.gen == a.waitGen
	if match {
		a.proc = nil
	}
	a.mu.Unlock()
	if !match {
		return
	}

	if wr.err != nil {
		log.Printf("[actor] %s wait: %v", a.id, wr.err)
		return
	}
	if wr.state != nil {
		log.Printf("[actor] %s exited: %s", a.id, wr.state)
	}
}
