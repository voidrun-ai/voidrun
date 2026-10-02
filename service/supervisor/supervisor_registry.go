package supervisor

import (
	"context"
	"errors"
	"sync"
	"time"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

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
	// BeforeWake prepares files for a sleeping sandbox. EE downloads and unpacks.
	BeforeWake(ctx context.Context, sb *model.Sandbox) error
	// AfterWake runs after a successful wake. EE drops the local pack and remote object.
	AfterWake(ctx context.Context, sb *model.Sandbox)
}

// LifecycleListener is called on the supervisor after a lifecycle event is published.
// Must not call Supervisor.Do / Snapshot / Delete (deadlock).
type LifecycleListener func(ev model.LifecycleEvent)

// SupervisorRegistry maps sandbox ID to a per-sandbox supervisor.
type SupervisorRegistry struct {
	mu            sync.Mutex
	supervisors   map[string]*Supervisor
	admission     Admission
	onProcessExit func(id string)
	listeners     []LifecycleListener
	onCount       func(int)
	onQueueWait   func(kind string, d time.Duration)
	life          *LifecycleDispatcher
}

func NewSupervisorRegistry() *SupervisorRegistry {
	r := &SupervisorRegistry{
		supervisors: make(map[string]*Supervisor),
	}
	r.life = newLifecycleDispatcher(r.lifeListeners)
	return r
}

// SetAdmission installs the lifecycle plugin. Nil clears it.
func (r *SupervisorRegistry) SetAdmission(a Admission) {
	r.mu.Lock()
	r.admission = a
	r.mu.Unlock()
}

func (r *SupervisorRegistry) adm() Admission {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.admission
}

// SetOnProcessExit installs a hook called after a matching process.Wait.
// Nil clears it.
func (r *SupervisorRegistry) SetOnProcessExit(fn func(id string)) {
	r.mu.Lock()
	r.onProcessExit = fn
	r.mu.Unlock()
}

func (r *SupervisorRegistry) notifyProcessExit(id string) {
	r.mu.Lock()
	fn := r.onProcessExit
	r.mu.Unlock()
	if fn != nil {
		fn(id)
	}
}

// AddLifecycleListener registers a subscriber. Wire at process start.
func (r *SupervisorRegistry) AddLifecycleListener(fn LifecycleListener) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.listeners = append(r.listeners, fn)
	r.mu.Unlock()
}

// SetObservers installs supervisor-count and queue-wait hooks. Nil clears a hook.
func (r *SupervisorRegistry) SetObservers(count func(int), wait func(kind string, d time.Duration)) {
	r.mu.Lock()
	r.onCount = count
	r.onQueueWait = wait
	r.mu.Unlock()
}

func (r *SupervisorRegistry) noteCount() {
	r.mu.Lock()
	n := len(r.supervisors)
	fn := r.onCount
	r.mu.Unlock()
	if fn != nil {
		fn(n)
	}
}

func (r *SupervisorRegistry) noteQueueWait(kind string, d time.Duration) {
	r.mu.Lock()
	fn := r.onQueueWait
	r.mu.Unlock()
	if fn != nil {
		fn(kind, d)
	}
}

func (r *SupervisorRegistry) lifeListeners() []LifecycleListener {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LifecycleListener, len(r.listeners))
	copy(out, r.listeners)
	return out
}

// Has reports whether a supervisor is registered for id.
func (r *SupervisorRegistry) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.supervisors[id]
	return ok
}

// SupervisesProcess is true when the supervisor for id is waiting on a child or
// watching an adopted pid. Health skips only then — Has(id) is not enough.
func (r *SupervisorRegistry) SupervisesProcess(id string) bool {
	return r.Get(id).SupervisesProcess()
}

// Get returns the registered supervisor, or nil.
func (r *SupervisorRegistry) Get(id string) *Supervisor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.supervisors[id]
}

// Emit publishes a lifecycle event without creating a supervisor. Uses the
// registered supervisor for generation when present; publishes directly otherwise
// so a stopped supervisor (post-Delete/fail paths) is not resurrected.
func (r *SupervisorRegistry) Emit(ctx context.Context, id string, ev model.LifecycleEvent) {
	if a := r.Get(id); a != nil {
		a.EmitLifecycle(ctx, ev)
		return
	}
	ev.SandboxID = id
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if ev.EventID == "" {
		ev.EventID = primitive.NewObjectID().Hex()
	}
	r.publishLifecycle(ctx, ev)
}

// GetOrCreate returns the supervisor for id, starting its loop on first use.
func (r *SupervisorRegistry) GetOrCreate(id string) *Supervisor {
	r.mu.Lock()
	if a, ok := r.supervisors[id]; ok {
		r.mu.Unlock()
		return a
	}
	a := newSupervisor(id, r)
	r.supervisors[id] = a
	r.mu.Unlock()
	go a.loop()
	r.noteCount()
	return a
}

// Unregister stops the supervisor for id and drops it from the map. No-op if absent.
func (r *SupervisorRegistry) Unregister(id string) {
	r.mu.Lock()
	a, ok := r.supervisors[id]
	if ok {
		delete(r.supervisors, id)
	}
	r.mu.Unlock()
	if ok {
		a.StopIdleTimer()
		a.shutdown()
		r.noteCount()
	}
}

func (r *SupervisorRegistry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.supervisors)
}

func (r *SupervisorRegistry) BeforeCreate(ctx context.Context, cpu, mem, diskMB int) error {
	a := r.adm()
	if a == nil {
		return nil
	}
	return a.BeforeCreate(ctx, cpu, mem, diskMB)
}

func (r *SupervisorRegistry) AfterCreate(cpu, mem, diskMB int) {
	if a := r.adm(); a != nil {
		a.AfterCreate(cpu, mem, diskMB)
	}
}

func (r *SupervisorRegistry) AfterCreateFailed(cpu, mem, diskMB int) {
	if a := r.adm(); a != nil {
		a.AfterCreateFailed(cpu, mem, diskMB)
	}
}
