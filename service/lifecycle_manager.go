package service

import (
	"context"
	"log"
	"time"

	"voidrun/config"
	"voidrun/metrics"
	"voidrun/repository"
	"voidrun/runtime"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Snapshotter is the subset of SandboxService used by the sleep sweeper.
// Implementations must be goroutine-safe and serialize per sandbox (supervisor inbox).
type Snapshotter interface {
	ApplySleep(ctx context.Context, orgID primitive.ObjectID, id string) error
}

// LifecycleManager sweeps this node's snapshotted and archived sandboxes.
// Idle snapshot stays on the supervisor. Delete runs here. Pack and S3 upload run in the EE packer.
type LifecycleManager struct {
	repo        repository.ISandboxRepository
	cfg         config.AutoLifecycleConfig
	hostID      *string
	monitor     *runtime.EventMonitor
	metrics     *metrics.Manager
	snapshotter Snapshotter
	sem         chan struct{}
}

// NewLifecycleManager wires the sweeper. snapshotter must be the SandboxService
// used for manual lifecycle ops so auto and API flows share the supervisor inbox.
// hostID scopes scans to this node's sandboxes only.
func NewLifecycleManager(
	cfg config.AutoLifecycleConfig,
	hostID *string,
	repo repository.ISandboxRepository,
	monitor *runtime.EventMonitor,
	metricsManager *metrics.Manager,
	snapshotter Snapshotter,
) *LifecycleManager {
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 10
	}
	return &LifecycleManager{
		repo:        repo,
		cfg:         cfg,
		hostID:      hostID,
		monitor:     monitor,
		metrics:     metricsManager,
		snapshotter: snapshotter,
		sem:         make(chan struct{}, conc),
	}
}

func sleepJobTimeout(act sleepAct) time.Duration {
	if act == sleepArchive {
		return time.Minute
	}
	return 30 * time.Minute
}

func (m *LifecycleManager) nodeID() string {
	if m == nil || m.hostID == nil {
		return ""
	}
	return *m.hostID
}

// Start launches the sleep-stage scan loop in a background goroutine.
func (m *LifecycleManager) Start(ctx context.Context) {
	if m == nil || !m.cfg.Enabled {
		log.Println("[lifecycle] auto-lifecycle management is disabled")
		return
	}

	intervalSec := m.cfg.CheckIntervalSec
	if intervalSec <= 0 {
		intervalSec = 30
	}
	interval := time.Duration(intervalSec) * time.Second

	log.Printf("[lifecycle] started host=%s (check every %s, delete=%ds)",
		m.nodeID(), interval, m.cfg.DeleteAfterSnapshottedSec)

	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Println("[lifecycle] stopped")
				return
			case <-ticker.C:
				m.sweepSleep(ctx)
			}
		}
	}()
}

func (m *LifecycleManager) sweepSleep(ctx context.Context) {
	if m == nil || m.repo == nil || m.snapshotter == nil {
		return
	}
	sandboxes, err := m.repo.FindSleeping(ctx, m.nodeID())
	if err != nil {
		log.Printf("[lifecycle] sleep sweep query failed: %v", err)
		return
	}

	now := time.Now()
	for _, sb := range sandboxes {
		if sb == nil {
			continue
		}
		act := dueSleepAction(sb, m.cfg.DeleteAfterSnapshottedSec, 0, 0, false, now)
		if act == sleepNone {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case m.sem <- struct{}{}:
		default:
			continue
		}
		go func() {
			defer func() { <-m.sem }()
			jobCtx, cancel := context.WithTimeout(context.Background(), sleepJobTimeout(act))
			defer cancel()
			if err := m.snapshotter.ApplySleep(jobCtx, sb.OrgID, sb.ID.Hex()); err != nil {
				log.Printf("[lifecycle] sleep sweep failed for %s: %v", sb.ID.Hex(), err)
			}
		}()
	}
}
