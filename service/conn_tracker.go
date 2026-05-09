package service

import (
	"sync"
	"sync/atomic"
)

// ConnTracker tracks active connection counts per sandbox.
// Used by the lifecycle manager to prevent pausing sandboxes with live connections.
type ConnTracker struct {
	mu       sync.Mutex
	counters map[string]*atomic.Int64
}

// NewConnTracker creates a new connection tracker.
func NewConnTracker() *ConnTracker {
	return &ConnTracker{
		counters: make(map[string]*atomic.Int64),
	}
}

// Acquire increments the active connection count for a sandbox.
func (ct *ConnTracker) Acquire(sandboxID string) {
	ct.getOrCreate(sandboxID).Add(1)
}

// Release decrements the active connection count for a sandbox.
func (ct *ConnTracker) Release(sandboxID string) {
	counter := ct.getOrCreate(sandboxID)
	if counter.Load() > 0 {
		counter.Add(-1)
	}
}

// ActiveCount returns the current active connection count for a sandbox.
func (ct *ConnTracker) ActiveCount(sandboxID string) int64 {
	ct.mu.Lock()
	counter, ok := ct.counters[sandboxID]
	ct.mu.Unlock()
	if !ok {
		return 0
	}
	return counter.Load()
}

// IsIdle returns true if the sandbox has zero active connections.
func (ct *ConnTracker) IsIdle(sandboxID string) bool {
	return ct.ActiveCount(sandboxID) == 0
}

// Remove cleans up the counter entry for a deleted sandbox.
func (ct *ConnTracker) Remove(sandboxID string) {
	ct.mu.Lock()
	delete(ct.counters, sandboxID)
	ct.mu.Unlock()
}

func (ct *ConnTracker) getOrCreate(sandboxID string) *atomic.Int64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	counter, ok := ct.counters[sandboxID]
	if !ok {
		counter = &atomic.Int64{}
		ct.counters[sandboxID] = counter
	}
	return counter
}
