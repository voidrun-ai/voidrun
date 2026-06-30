package service

import "sync"

// SandboxLifecycleLocks provides per-sandbox-ID mutual exclusion for lifecycle
// operations (Snapshot, Restore, Delete). Entries are refcounted and removed
// when no longer held.
type SandboxLifecycleLocks struct {
	mu    sync.Mutex
	locks map[string]*lifecycleLockEntry
}

type lifecycleLockEntry struct {
	mu       sync.Mutex
	refCount int
}

func NewSandboxLifecycleLocks() *SandboxLifecycleLocks {
	return &SandboxLifecycleLocks{
		locks: make(map[string]*lifecycleLockEntry),
	}
}

// Acquire blocks until the lock for id is held, then returns a release fn
// that MUST be called exactly once (typically via defer).
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
