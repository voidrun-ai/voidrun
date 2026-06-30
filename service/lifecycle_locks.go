package service

import "sync"

// SandboxLifecycleLocks provides per-sandbox-ID mutual exclusion for the lifecycle
// operations (Snapshot, Restore, Delete, auto-snapshot, auto-delete). Without it,
// concurrent operations on the same sandbox can spawn duplicate VMM processes,
// corrupt snapshot directories, kill processes whose PIDs were reused, or leave
// the DB status mismatched with the runtime.
//
// Entries are reference-counted and removed from the map when no longer held, so
// the working set stays proportional to in-flight operations rather than the
// total number of sandboxes the process has ever touched.
type SandboxLifecycleLocks struct {
	mu    sync.Mutex
	locks map[string]*lifecycleLockEntry
}

type lifecycleLockEntry struct {
	mu       sync.Mutex
	refCount int
}

// NewSandboxLifecycleLocks constructs an empty locker.
func NewSandboxLifecycleLocks() *SandboxLifecycleLocks {
	return &SandboxLifecycleLocks{
		locks: make(map[string]*lifecycleLockEntry),
	}
}

// Acquire blocks until the lifecycle lock for id is held by this caller, then
// returns a release function. The release function MUST be called exactly once;
// the typical use is `defer release()` immediately after Acquire.
//
// The map is protected by SandboxLifecycleLocks.mu only while inspecting or
// mutating the entry table. The per-id mutex is held independently, so two
// different sandbox IDs never contend.
func (s *SandboxLifecycleLocks) Acquire(id string) func() {
	s.mu.Lock()
	entry, ok := s.locks[id]
	if !ok {
		entry = &lifecycleLockEntry{}
		s.locks[id] = entry
	}
	entry.refCount++
	s.mu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()
		s.mu.Lock()
		entry.refCount--
		if entry.refCount == 0 {
			delete(s.locks, id)
		}
		s.mu.Unlock()
	}
}
