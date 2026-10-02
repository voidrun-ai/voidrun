package supervisor

import (
	"context"
	"log"
	"sync"
	"time"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// LifecycleDispatcher delivers one sandbox's listener calls in emit order.
// A single drain goroutine keeps committed work off the supervisor loop.
// Starting events wait so a later event cannot pass an earlier one.
type LifecycleDispatcher struct {
	mu    sync.Mutex
	lanes map[string]*lifeLane
	// gens outlives a dropped lane so the next event for an id stays above the last one.
	gens      map[string]uint64
	listeners func() []LifecycleListener
}

func newLifecycleDispatcher(listeners func() []LifecycleListener) *LifecycleDispatcher {
	return &LifecycleDispatcher{
		lanes:     make(map[string]*lifeLane),
		gens:      make(map[string]uint64),
		listeners: listeners,
	}
}

func (d *LifecycleDispatcher) count() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.lanes)
}

type lifeLane struct {
	id    string
	mu    sync.Mutex
	queue []lifeNote
	busy  bool
}

type lifeNote struct {
	ev   model.LifecycleEvent
	done chan struct{}
}

func (r *SupervisorRegistry) laneCount() int {
	if r == nil || r.life == nil {
		return 0
	}
	return r.life.count()
}

func (r *SupervisorRegistry) publishLifecycle(_ context.Context, ev model.LifecycleEvent) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if ev.EventID == "" {
		ev.EventID = primitive.NewObjectID().Hex()
	}
	log.Printf("[supervisor] %s %s %s -> %s", ev.SandboxID, ev.EventName(), ev.FromStatus, ev.ToStatus)
	if r.life == nil {
		return
	}
	r.life.deliver(ev)
}

func (d *LifecycleDispatcher) deliver(ev model.LifecycleEvent) {
	var done chan struct{}
	if ev.Phase == model.PhaseStarting {
		done = make(chan struct{})
	}
	d.enqueue(lifeNote{ev: ev, done: done})
	if done != nil {
		<-done
	}
}

func (d *LifecycleDispatcher) enqueue(n lifeNote) {
	d.mu.Lock()
	lane := d.lanes[n.ev.SandboxID]
	if lane == nil {
		lane = &lifeLane{id: n.ev.SandboxID}
		d.lanes[n.ev.SandboxID] = lane
	}
	d.gens[n.ev.SandboxID]++
	n.ev.Generation = d.gens[n.ev.SandboxID]
	lane.mu.Lock()
	lane.queue = append(lane.queue, n)
	start := !lane.busy
	lane.busy = true
	lane.mu.Unlock()
	d.mu.Unlock()
	if start {
		go d.drain(lane)
	}
}

func (d *LifecycleDispatcher) drain(lane *lifeLane) {
	for {
		lane.mu.Lock()
		if len(lane.queue) == 0 {
			lane.busy = false
			lane.mu.Unlock()
			if d.releaseLane(lane) {
				return
			}
			continue
		}
		n := lane.queue[0]
		lane.queue[0] = lifeNote{}
		lane.queue = lane.queue[1:]
		lane.mu.Unlock()
		d.invoke(n.ev)
		if n.done != nil {
			close(n.done)
		}
	}
}

// releaseLane drops the lane once it is idle. It returns false when a note
// arrived after the drain decided the queue was empty, so the same goroutine
// keeps draining.
func (d *LifecycleDispatcher) releaseLane(lane *lifeLane) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if d.lanes[lane.id] != lane {
		return true
	}
	if len(lane.queue) == 0 && !lane.busy {
		delete(d.lanes, lane.id)
		return true
	}
	if len(lane.queue) > 0 && !lane.busy {
		lane.busy = true
		return false
	}
	return true
}

func (d *LifecycleDispatcher) invoke(ev model.LifecycleEvent) {
	if d.listeners == nil {
		return
	}
	for _, fn := range d.listeners() {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[supervisor] %s listener panic: %v", ev.SandboxID, rec)
				}
			}()
			fn(ev)
		}()
	}
}
