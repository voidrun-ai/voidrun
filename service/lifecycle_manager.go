package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"voidrun/config"
	"voidrun/metrics"
	"voidrun/repository"
	"voidrun/runtime"
)

// LifecycleManager runs periodic scans to auto-snapshot and auto-delete sandboxes.
type LifecycleManager struct {
	repo      repository.ISandboxRepository
	cfg       config.AutoLifecycleConfig
	defaultHV string
	monitor   *runtime.EventMonitor
	metrics   *metrics.Manager
}

// NewLifecycleManager creates a new lifecycle manager.
func NewLifecycleManager(
	cfg config.AutoLifecycleConfig,
	defaultHV string,
	repo repository.ISandboxRepository,
	monitor *runtime.EventMonitor,
	metricsManager *metrics.Manager,
) *LifecycleManager {
	return &LifecycleManager{
		repo:      repo,
		cfg:       cfg,
		defaultHV: defaultHV,
		monitor:   monitor,
		metrics:   metricsManager,
	}
}

func (m *LifecycleManager) resolveHV(hv string) string {
	if hv != "" {
		return hv
	}
	return m.defaultHV
}

// Start launches the lifecycle scan loop in a background goroutine.
func (m *LifecycleManager) Start(ctx context.Context) {
	if !m.cfg.Enabled {
		log.Println("[lifecycle] auto-lifecycle management is disabled")
		return
	}

	intervalSec := m.cfg.CheckIntervalSec
	if intervalSec <= 0 {
		intervalSec = 30
	}
	interval := time.Duration(intervalSec) * time.Second

	log.Printf("[lifecycle] started (check every %s, snapshot-idle=%ds, delete-snapshotted=%ds)",
		interval, m.cfg.SnapshotAfterIdleSec, m.cfg.DeleteAfterSnapshottedSec)

	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Println("[lifecycle] stopped")
				return
			case <-ticker.C:
				m.tick(ctx)
			}
		}
	}()
}

func (m *LifecycleManager) tick(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		m.autoSnapshot(ctx)
	}()

	go func() {
		defer wg.Done()
		m.autoDelete(ctx)
	}()

	wg.Wait()
}

// autoSnapshot snapshots running sandboxes that have been idle too long.
func (m *LifecycleManager) autoSnapshot(ctx context.Context) {
	if m.cfg.SnapshotAfterIdleSec <= 0 {
		return
	}

	threshold := time.Now().Add(-time.Duration(m.cfg.SnapshotAfterIdleSec) * time.Second)
	sandboxes, err := m.repo.FindIdleRunning(ctx, threshold)
	if err != nil {
		log.Printf("[lifecycle] auto-snapshot query failed: %v", err)
		return
	}

	maxConc := m.cfg.Concurrency
	if maxConc <= 0 {
		maxConc = 10
	}
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup

	for _, sb := range sandboxes {
		sb := sb
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer func() { <-sem; wg.Done() }()

			id := sb.ID.Hex()

			// Stop event monitor BEFORE snapshotting so it can do a final sync
			// while the CLH API socket is still alive.
			if m.monitor != nil {
				m.monitor.Stop(ctx, id)
			}

			snapshotDir, err := runtime.PrepareSnapshotDir(id)
			if err != nil {
				log.Printf("[lifecycle] auto-snapshot dir failed for %s (%s): %v", sb.Name, id, err)
				return
			}
			if err := runtime.Snapshot(ctx, id, snapshotDir, m.resolveHV(sb.Hypervisor)); err != nil {
				log.Printf("[lifecycle] auto-snapshot runtime failed for %s (%s): %v", sb.Name, id, err)
				return
			}
			if m.metrics != nil {
				m.metrics.UnregisterSandbox(id)
			}
			if err := m.repo.SetSnapshottedAt(ctx, sb.ID); err != nil {
				log.Printf("[lifecycle] auto-snapshot DB update failed for %s (%s): %v", sb.Name, id, err)
				return
			}
			log.Printf("[lifecycle] auto-snapshotted sandbox %s (%s) after %ds idle", sb.Name, id, m.cfg.SnapshotAfterIdleSec)
		}()
	}
	wg.Wait()
}

// autoDelete deletes snapshotted sandboxes that have been snapshotted too long.
func (m *LifecycleManager) autoDelete(ctx context.Context) {
	if m.cfg.DeleteAfterSnapshottedSec <= 0 {
		return
	}

	threshold := time.Now().Add(-time.Duration(m.cfg.DeleteAfterSnapshottedSec) * time.Second)
	sandboxes, err := m.repo.FindStaleSnapshotted(ctx, threshold)
	if err != nil {
		log.Printf("[lifecycle] auto-delete query failed: %v", err)
		return
	}

	maxConc := m.cfg.Concurrency
	if maxConc <= 0 {
		maxConc = 10
	}
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup

	for _, sb := range sandboxes {
		sb := sb
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer func() { <-sem; wg.Done() }()

			id := sb.ID.Hex()

			if err := runtime.Delete(ctx, id, sb.TapName, sb.NetNSName, m.resolveHV(sb.Hypervisor)); err != nil {
				log.Printf("[lifecycle] auto-delete runtime failed for %s (%s): %v", sb.Name, id, err)
				// Continue with cleanup anyway — the VM may already be gone
			}

			// Stop event monitor (final sync)
			if m.monitor != nil {
				m.monitor.Stop(ctx, id)
			}

			// Physical cleanup
			if err := runtime.Cleanup(id); err != nil {
				fmt.Printf("[lifecycle] auto-delete cleanup failed for %s (%s): %v\n", sb.Name, id, err)
			}

			// Mark as deleted in DB
			if err := m.repo.UpdateStatusForHealth(ctx, sb.ID, "deleted"); err != nil {
				log.Printf("[lifecycle] auto-delete DB update failed for %s (%s): %v", sb.Name, id, err)
				return
			}
			log.Printf("[lifecycle] auto-deleted sandbox %s (%s) after %ds snapshotted", sb.Name, id, m.cfg.DeleteAfterSnapshottedSec)
		}()
	}
	wg.Wait()
}
