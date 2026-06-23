package runtime

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// EventSink is the interface used by the monitor to persist events.
// Implemented by EventRepository.
type EventSink interface {
	SaveEvents(ctx context.Context, events []*model.SandboxEvent) error
}

// WatcherConfig holds tunable parameters for a sandboxWatcher.
type WatcherConfig struct {
	PollInterval    time.Duration // how often to read the event source
	BatchSize       int           // max events flushed per poll
	MaxConsecErrors int           // stop goroutine after this many consecutive sink errors
}

func defaultWatcherConfig() WatcherConfig {
	return WatcherConfig{
		PollInterval:    100 * time.Millisecond,
		BatchSize:       50,
		MaxConsecErrors: 10,
	}
}

// SandboxMeta carries the minimal DB data needed to resume a watcher on server restart.
type SandboxMeta struct {
	SandboxID  primitive.ObjectID
	OrgID      primitive.ObjectID
	UserID     primitive.ObjectID // CreatedBy
	Hypervisor string             // canonical hypervisor name; empty for legacy rows
}

// sandboxWatcher tails a single VM's event source and pushes records into the
// EventSink as model.SandboxEvent rows.
type sandboxWatcher struct {
	sandboxID primitive.ObjectID
	orgID     primitive.ObjectID
	userID    primitive.ObjectID
	src       EventSource
	sink      EventSink
	cfg       WatcherConfig
	cancel    context.CancelFunc
}

func newSandboxWatcher(meta SandboxMeta, src EventSource, sink EventSink, cfg WatcherConfig) *sandboxWatcher {
	return &sandboxWatcher{
		sandboxID: meta.SandboxID,
		orgID:     meta.OrgID,
		userID:    meta.UserID,
		src:       src,
		sink:      sink,
		cfg:       cfg,
	}
}

func (w *sandboxWatcher) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	go w.run(ctx)
}

func (w *sandboxWatcher) stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *sandboxWatcher) loadOffset() int64 {
	data, err := os.ReadFile(w.src.OffsetPath())
	if err != nil {
		return 0
	}
	off, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return off
}

func (w *sandboxWatcher) saveOffset(offset int64) {
	tmp := w.src.OffsetPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)), 0644); err != nil {
		log.Printf("[event_monitor] failed to write offset tmp for %s: %v", w.sandboxID.Hex(), err)
		return
	}
	if err := os.Rename(tmp, w.src.OffsetPath()); err != nil {
		log.Printf("[event_monitor] failed to rename offset file for %s: %v", w.sandboxID.Hex(), err)
	}
}

func (w *sandboxWatcher) run(ctx context.Context) {
	id := w.sandboxID.Hex()
	log.Printf("[event_monitor] watcher started for sandbox %s (source=%s)", id, w.src.Source())
	defer log.Printf("[event_monitor] watcher stopped for sandbox %s", id)

	offset := w.loadOffset()
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	consecErrors := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newOffset, err := w.poll(ctx, offset)
			if err != nil {
				consecErrors++
				log.Printf("[event_monitor] poll error for %s (%d/%d): %v",
					id, consecErrors, w.cfg.MaxConsecErrors, err)
				if consecErrors >= w.cfg.MaxConsecErrors {
					log.Printf("[event_monitor] too many errors for %s, stopping watcher", id)
					return
				}
				continue
			}
			if newOffset != offset {
				w.saveOffset(newOffset)
				offset = newOffset
			}
			consecErrors = 0
		}
	}
}

func (w *sandboxWatcher) poll(ctx context.Context, offset int64) (int64, error) {
	records, newOffset, err := w.src.Poll(ctx, offset)
	if err != nil {
		return offset, err
	}
	if len(records) == 0 {
		return newOffset, nil
	}

	// Convert records to model.SandboxEvent rows and finalise timestamps.
	batch := make([]*model.SandboxEvent, 0, len(records))
	for _, rec := range records {
		ev := &model.SandboxEvent{
			ID:        primitive.NewObjectID(),
			SandboxID: w.sandboxID,
			OrgID:     w.orgID,
			UserID:    w.userID,
			Event:     rec.Event,
			Source:    w.src.Source(),
			Timestamp: time.Now(),
			Meta:      map[string]any{"uptime_ns": rec.UptimeNs},
		}
		for k, v := range rec.Properties {
			ev.Meta[k] = v
		}
		batch = append(batch, ev)
	}
	finalizeTimestamps(batch)

	// Flush in chunks of BatchSize.
	for start := 0; start < len(batch); start += w.cfg.BatchSize {
		end := start + w.cfg.BatchSize
		if end > len(batch) {
			end = len(batch)
		}
		if err := w.sink.SaveEvents(ctx, batch[start:end]); err != nil {
			return offset, fmt.Errorf("save events batch: %w", err)
		}
	}
	return newOffset, nil
}

// finalizeTimestamps adjusts the event timestamps in a batch. Since hypervisors
// only give uptime, we assume the last event happened "now" and back-fill the
// rest relative to it.
func finalizeTimestamps(batch []*model.SandboxEvent) {
	if len(batch) == 0 {
		return
	}
	now := time.Now()
	lastUptimeRaw, _ := batch[len(batch)-1].Meta["uptime_ns"].(int64)
	lastUptime := time.Duration(lastUptimeRaw) * time.Nanosecond
	for _, ev := range batch {
		uptimeRaw, _ := ev.Meta["uptime_ns"].(int64)
		uptime := time.Duration(uptimeRaw) * time.Nanosecond
		ev.Timestamp = now.Add(-(lastUptime - uptime))
	}
}

// -----------------------------------------------------------------------------
// EventMonitor — manages the lifecycle of all per-VM watchers
// -----------------------------------------------------------------------------

// EventMonitor starts, stops, and resumes watchers for all sandboxes. It is
// hypervisor-agnostic: the EventSource is provided by the active Hypervisor.
type EventMonitor struct {
	mu       sync.Mutex
	watchers map[string]*sandboxWatcher
	sink     EventSink
	cfg      WatcherConfig
	rootCtx  context.Context
	resolver HypervisorResolver
}

// NewEventMonitor creates a monitor. The resolver determines which hypervisor
// EventSource to attach for each sandbox.
func NewEventMonitor(sink EventSink, resolver HypervisorResolver) *EventMonitor {
	return &EventMonitor{
		watchers: make(map[string]*sandboxWatcher),
		sink:     sink,
		cfg:      defaultWatcherConfig(),
		resolver: resolver,
	}
}

func (m *EventMonitor) WithConfig(cfg WatcherConfig) *EventMonitor {
	m.cfg = cfg
	return m
}

func (m *EventMonitor) SetRootContext(ctx context.Context) {
	m.mu.Lock()
	m.rootCtx = ctx
	m.mu.Unlock()
}

// Start begins watching the event source for a newly created sandbox.
// hypervisorName selects the backend whose EventSource will be attached; if
// empty, the resolver's default is used.
func (m *EventMonitor) Start(ctx context.Context, sbxID primitive.ObjectID, orgID, userID primitive.ObjectID, hypervisorName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := sbxID.Hex()
	if _, exists := m.watchers[id]; exists {
		return
	}
	parent := m.rootCtx
	if parent == nil {
		parent = ctx
	}

	src := m.eventSourceFor(id, hypervisorName)
	if src == nil {
		return
	}
	w := newSandboxWatcher(SandboxMeta{
		SandboxID:  sbxID,
		OrgID:      orgID,
		UserID:     userID,
		Hypervisor: hypervisorName,
	}, src, m.sink, m.cfg)
	w.start(parent)
	m.watchers[id] = w
}

// Stop cancels the watcher for the given sandbox (called on delete).
func (m *EventMonitor) Stop(ctx context.Context, sbxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if w, ok := m.watchers[sbxID]; ok {
		offset := w.loadOffset()
		if newOffset, err := w.poll(ctx, offset); err == nil && newOffset != offset {
			w.saveOffset(newOffset)
		}
		w.stop()
		delete(m.watchers, sbxID)
	}
}

// ResumeAll re-attaches watchers to sandboxes that were active before a
// server restart. Safe to call at startup before any HTTP traffic.
func (m *EventMonitor) ResumeAll(ctx context.Context, sandboxes []SandboxMeta) {
	m.mu.Lock()
	defer m.mu.Unlock()

	parent := m.rootCtx
	if parent == nil {
		parent = ctx
	}

	resumed := 0
	for _, meta := range sandboxes {
		id := meta.SandboxID.Hex()
		if _, exists := m.watchers[id]; exists {
			continue
		}
		src := m.eventSourceFor(id, meta.Hypervisor)
		if src == nil {
			continue
		}
		// Only resume if there's an on-disk source to read from.
		if _, err := os.Stat(src.OffsetPath()); os.IsNotExist(err) {
			// Offset file missing is fine; only skip if the data file itself
			// also doesn't exist. CLH event file path is reachable via the
			// resolver-specific source.Poll, which returns (nil, off, nil)
			// gracefully when the data file is absent — so we attach anyway
			// and let the watcher idle until the hypervisor writes events.
		}
		w := newSandboxWatcher(meta, src, m.sink, m.cfg)
		w.start(parent)
		m.watchers[id] = w
		resumed++
	}
	if resumed > 0 {
		log.Printf("[event_monitor] resumed %d watcher(s) after server restart", resumed)
	}
}

func (m *EventMonitor) eventSourceFor(id, hypervisorName string) EventSource {
	if m.resolver == nil {
		return nil
	}
	var hv Hypervisor
	if hypervisorName == "" {
		hv = m.resolver.Default()
	} else {
		// Build a transient sandbox shim for the resolver. We avoid importing
		// the full Sandbox model here to keep the signature small.
		hv = m.resolver.For(&model.Sandbox{Hypervisor: hypervisorName})
	}
	if hv == nil {
		return nil
	}
	return hv.EventSource(id)
}
