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

// LifecycleManager runs periodic scans to auto-pause, auto-stop, and auto-delete sandboxes.
type LifecycleManager struct {
	repo           repository.ISandboxRepository
	cfg            config.AutoLifecycleConfig
	defaultHV      string
	monitor        *runtime.EventMonitor
	metrics        *metrics.Manager
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

	log.Printf("[lifecycle] started (check every %s, pause-idle=%ds, stop-paused=%ds, delete-stopped=%ds)",
		interval, m.cfg.PauseAfterIdleSec, m.cfg.StopAfterPausedSec, m.cfg.DeleteAfterStoppedSec)

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
	wg.Add(3)

	go func() {
		defer wg.Done()
		m.autoPause(ctx)
	}()

	go func() {
		defer wg.Done()
		m.autoStop(ctx)
	}()

	go func() {
		defer wg.Done()
		m.autoDelete(ctx)
	}()

	wg.Wait()
}

// autoPause pauses running sandboxes that have been idle too long.
func (m *LifecycleManager) autoPause(ctx context.Context) {
	if m.cfg.PauseAfterIdleSec <= 0 {
		return
	}

	threshold := time.Now().Add(-time.Duration(m.cfg.PauseAfterIdleSec) * time.Second)
	sandboxes, err := m.repo.FindIdleRunning(ctx, threshold)
	if err != nil {
		log.Printf("[lifecycle] auto-pause query failed: %v", err)
		return
	}

	for _, sb := range sandboxes {
		id := sb.ID.Hex()
		if err := runtime.Pause(ctx, id, m.resolveHV(sb.Hypervisor)); err != nil {
			log.Printf("[lifecycle] auto-pause runtime failed for %s (%s): %v", sb.Name, id, err)
			continue
		}
		if err := m.repo.SetPausedAt(ctx, sb.ID); err != nil {
			log.Printf("[lifecycle] auto-pause DB update failed for %s (%s): %v", sb.Name, id, err)
			continue
		}
		log.Printf("[lifecycle] auto-paused sandbox %s (%s) after %ds idle", sb.Name, id, m.cfg.PauseAfterIdleSec)
	}
}

// autoStop stops paused sandboxes that have been paused too long.
func (m *LifecycleManager) autoStop(ctx context.Context) {
	if m.cfg.StopAfterPausedSec <= 0 {
		return
	}

	threshold := time.Now().Add(-time.Duration(m.cfg.StopAfterPausedSec) * time.Second)
	sandboxes, err := m.repo.FindStalePaused(ctx, threshold)
	if err != nil {
		log.Printf("[lifecycle] auto-stop query failed: %v", err)
		return
	}

	for _, sb := range sandboxes {
		id := sb.ID.Hex()
		if err := runtime.Stop(ctx, id, m.resolveHV(sb.Hypervisor)); err != nil {
			log.Printf("[lifecycle] auto-stop runtime failed for %s (%s): %v", sb.Name, id, err)
			continue
		}
		if m.metrics != nil {
			m.metrics.UnregisterSandbox(id)
		}
		if err := m.repo.SetStoppedAt(ctx, sb.ID); err != nil {
			log.Printf("[lifecycle] auto-stop DB update failed for %s (%s): %v", sb.Name, id, err)
			continue
		}
		log.Printf("[lifecycle] auto-stopped sandbox %s (%s) after %ds paused", sb.Name, id, m.cfg.StopAfterPausedSec)
	}
}

// autoDelete deletes stopped sandboxes that have been stopped too long.
func (m *LifecycleManager) autoDelete(ctx context.Context) {
	if m.cfg.DeleteAfterStoppedSec <= 0 {
		return
	}

	threshold := time.Now().Add(-time.Duration(m.cfg.DeleteAfterStoppedSec) * time.Second)
	sandboxes, err := m.repo.FindStaleStopped(ctx, threshold)
	if err != nil {
		log.Printf("[lifecycle] auto-delete query failed: %v", err)
		return
	}

	for _, sb := range sandboxes {
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
			continue
		}
		log.Printf("[lifecycle] auto-deleted sandbox %s (%s) after %ds stopped", sb.Name, id, m.cfg.DeleteAfterStoppedSec)
	}
}
