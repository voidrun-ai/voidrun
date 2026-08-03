package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"voidrun/config"
	"voidrun/metrics"
	"voidrun/repository"
	"voidrun/runtime"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Snapshotter is the subset of SandboxService used by auto-snapshot.
// Implementations must be goroutine-safe and acquire their own per-sandbox lock.
type Snapshotter interface {
	Snapshot(ctx context.Context, orgID primitive.ObjectID, id string) error
}

// LifecycleManager runs periodic scans to auto-snapshot and auto-delete sandboxes.
type LifecycleManager struct {
	repo           repository.ISandboxRepository
	cfg            config.AutoLifecycleConfig
	monitor        *runtime.EventMonitor
	metrics        *metrics.Manager
	lifecycleLocks *SandboxLifecycleLocks
	snapshotter    Snapshotter
}

// NewLifecycleManager wires the sweeper. lifecycleLocks and snapshotter must
// be the same instances used by SandboxService so manual and auto flows serialize.
func NewLifecycleManager(
	cfg config.AutoLifecycleConfig,
	repo repository.ISandboxRepository,
	monitor *runtime.EventMonitor,
	metricsManager *metrics.Manager,
	lifecycleLocks *SandboxLifecycleLocks,
	snapshotter Snapshotter,
) *LifecycleManager {
	if lifecycleLocks == nil {
		lifecycleLocks = NewSandboxLifecycleLocks()
	}
	return &LifecycleManager{
		repo:           repo,
		cfg:            cfg,
		monitor:        monitor,
		metrics:        metricsManager,
		lifecycleLocks: lifecycleLocks,
		snapshotter:    snapshotter,
	}
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

			// Delegate to the public Snapshot path so manual + auto flows can't drift.
			// Races against concurrent transitions surface as ErrSandboxNotFound /
			// ErrSandboxNotRunning and are expected here.
			if err := m.snapshotter.Snapshot(ctx, sb.OrgID, id); err != nil {
				switch {
				case errors.Is(err, ErrSandboxNotFound), errors.Is(err, ErrSandboxNotRunning):
					return
				default:
					log.Printf("[lifecycle] auto-snapshot failed for %s (%s): %v", sb.Name, id, err)
					return
				}
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

			// Serialize with manual lifecycle ops and the auto-snapshot sweep.
			release := m.lifecycleLocks.Acquire(id)
			defer release()

			current, err := m.repo.FindByID(ctx, sb.ID, options.FindOneOptions{})
			if err != nil {
				log.Printf("[lifecycle] auto-delete lookup failed for %s (%s): %v", sb.Name, id, err)
				return
			}
			if current == nil || current.Status != "snapshotted" {
				return
			}

			if err := runtime.Delete(id, sb.TapName, sb.NetNSName); err != nil {
				log.Printf("[lifecycle] auto-delete runtime failed for %s (%s): %v", sb.Name, id, err)
			}

			if m.monitor != nil {
				m.monitor.Stop(ctx, id)
			}

			if err := runtime.Cleanup(id); err != nil {
				fmt.Printf("[lifecycle] auto-delete cleanup failed for %s (%s): %v\n", sb.Name, id, err)
			}

			if err := m.repo.UpdateStatusFrom(ctx, sb.ID, "snapshotted", "deleted"); err != nil {
				log.Printf("[lifecycle] auto-delete DB update failed for %s (%s): %v", sb.Name, id, err)
				return
			}
			if m.metrics != nil {
				m.metrics.ClearSandboxStatus(id)
			}
			log.Printf("[lifecycle] auto-deleted sandbox %s (%s) after %ds snapshotted", sb.Name, id, m.cfg.DeleteAfterSnapshottedSec)
		}()
	}
	wg.Wait()
}
