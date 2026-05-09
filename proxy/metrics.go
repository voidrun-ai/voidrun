package proxy

import (
	"sync"
	"sync/atomic"
)

// Metrics tracks per-VM proxy usage using lock-free atomic counters.
type Metrics struct {
	mu    sync.RWMutex
	perVM map[string]*vmMetrics
}

type vmMetrics struct {
	Requests      atomic.Int64
	Bytes         atomic.Int64
	Blocked       atomic.Int64
	Errors        atomic.Int64
	Substitutions atomic.Int64
}

func NewMetrics() *Metrics {
	return &Metrics{
		perVM: make(map[string]*vmMetrics),
	}
}

func (m *Metrics) getOrCreate(vmIP string) *vmMetrics {
	m.mu.RLock()
	vm, ok := m.perVM[vmIP]
	m.mu.RUnlock()
	if ok {
		return vm
	}

	m.mu.Lock()
	// Double-check
	vm, ok = m.perVM[vmIP]
	if !ok {
		vm = &vmMetrics{}
		m.perVM[vmIP] = vm
	}
	m.mu.Unlock()
	return vm
}

func (m *Metrics) RecordRequest(vmIP, host string) {
	m.getOrCreate(vmIP).Requests.Add(1)
}

func (m *Metrics) RecordBlocked(vmIP, host string) {
	m.getOrCreate(vmIP).Blocked.Add(1)
}

func (m *Metrics) RecordError(vmIP, host string) {
	m.getOrCreate(vmIP).Errors.Add(1)
}

func (m *Metrics) RecordSubstitution(vmIP string) {
	m.getOrCreate(vmIP).Substitutions.Add(1)
}

func (m *Metrics) RecordBytes(vmIP string, n int64) {
	m.getOrCreate(vmIP).Bytes.Add(n)
}

// Snapshot returns a copy of all metrics for reporting.
type MetricsSnapshot struct {
	VMIP          string
	Requests      int64
	Bytes         int64
	Blocked       int64
	Errors        int64
	Substitutions int64
}

func (m *Metrics) Snapshot() []MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]MetricsSnapshot, 0, len(m.perVM))
	for ip, vm := range m.perVM {
		out = append(out, MetricsSnapshot{
			VMIP:          ip,
			Requests:      vm.Requests.Load(),
			Bytes:         vm.Bytes.Load(),
			Blocked:       vm.Blocked.Load(),
			Errors:        vm.Errors.Load(),
			Substitutions: vm.Substitutions.Load(),
		})
	}
	return out
}

// Remove cleans up metrics for a destroyed VM.
func (m *Metrics) Remove(vmIP string) {
	m.mu.Lock()
	delete(m.perVM, vmIP)
	m.mu.Unlock()
}
