package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CLHEventPayload mirrors the JSON that Cloud-Hypervisor writes to its event file.
type CLHEventPayload struct {
	Timestamp struct {
		Secs  int64 `json:"secs"`
		Nanos int64 `json:"nanos"`
	} `json:"timestamp"`
	Source     string `json:"source"` // "vm", "vmm", "virtio-device", "api"
	Event      string `json:"event"`
	Properties any    `json:"properties,omitempty"`
}

// EventSink is the interface used by the monitor to persist events.
// Implemented by EventRepository.
type EventSink interface {
	SaveEvents(ctx context.Context, events []*model.SandboxEvent) error
}

// WatcherConfig holds tunable parameters for a SandboxWatcher.
type WatcherConfig struct {
	PollInterval    time.Duration // how often to read the event file
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
	SandboxID primitive.ObjectID
	OrgID     primitive.ObjectID
	UserID    primitive.ObjectID // CreatedBy
}

// sandboxWatcher tails a single VM's event file and pushes events to the sink.
type sandboxWatcher struct {
	sandboxID primitive.ObjectID
	orgID     primitive.ObjectID
	userID    primitive.ObjectID
	eventPath string
	offPath   string
	sink      EventSink
	cfg       WatcherConfig
	cancel    context.CancelFunc
}

func newSandboxWatcher(meta SandboxMeta, sink EventSink, cfg WatcherConfig) *sandboxWatcher {
	id := meta.SandboxID.Hex()
	return &sandboxWatcher{
		sandboxID: meta.SandboxID,
		orgID:     meta.OrgID,
		userID:    meta.UserID,
		eventPath: GetEventPath(id),
		offPath:   GetEventOffsetPath(id),
		sink:      sink,
		cfg:       cfg,
	}
}

// start launches the tail loop in a goroutine.
func (w *sandboxWatcher) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	go w.run(ctx)
}

// stop cancels the watcher goroutine.
func (w *sandboxWatcher) stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

// loadOffset reads the saved byte offset, returns 0 if missing.
func (w *sandboxWatcher) loadOffset() int64 {
	data, err := os.ReadFile(w.offPath)
	if err != nil {
		return 0
	}
	off, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return off
}

// saveOffset writes the current byte offset to disk atomically (write-then-rename).
func (w *sandboxWatcher) saveOffset(offset int64) {
	tmp := w.offPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)), 0644); err != nil {
		log.Printf("[event_monitor] failed to write offset tmp for %s: %v", w.sandboxID.Hex(), err)
		return
	}
	if err := os.Rename(tmp, w.offPath); err != nil {
		log.Printf("[event_monitor] failed to rename offset file for %s: %v", w.sandboxID.Hex(), err)
	}
}

// run is the main poll loop.
func (w *sandboxWatcher) run(ctx context.Context) {
	id := w.sandboxID.Hex()
	log.Printf("[event_monitor] watcher started for sandbox %s", id)
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

// poll reads new JSON objects from offset, persists them, returns the new offset.
func (w *sandboxWatcher) poll(ctx context.Context, offset int64) (int64, error) {
	f, err := os.Open(w.eventPath)
	if err != nil {
		if os.IsNotExist(err) {
			return offset, nil // file not yet created by CLH, skip silently
		}
		return offset, fmt.Errorf("open event file: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, fmt.Errorf("seek to %d: %w", offset, err)
	}

	var batch []*model.SandboxEvent
	decoder := json.NewDecoder(f)

	for {
		var payload CLHEventPayload
		err := decoder.Decode(&payload)
		if err == io.EOF {
			break
		}
		if err != nil {
			// If we hit an error, it might be a partial write at the end of the file.
			log.Printf("[event_monitor] decode error for %s at offset %d: %v", w.sandboxID.Hex(), offset+decoder.InputOffset(), err)
			break
		}

		// Calculate absolute timestamp.
		uptime := time.Duration(payload.Timestamp.Secs)*time.Second + time.Duration(payload.Timestamp.Nanos)*time.Nanosecond

		ev := &model.SandboxEvent{
			ID:        primitive.NewObjectID(),
			SandboxID: w.sandboxID,
			OrgID:     w.orgID,
			UserID:    w.userID,
			Event:     payload.Event,
			Source:    "clh",
			Timestamp: time.Now(), // Placeholder, updated below
			Meta: map[string]any{
				"uptime_ns": uptime.Nanoseconds(),
			},
		}

		if payload.Properties != nil {
			if d, ok := payload.Properties.(map[string]any); ok {
				for k, v := range d {
					ev.Meta[k] = v
				}
			}
		}

		batch = append(batch, ev)

		if len(batch) >= w.cfg.BatchSize {
			w.finalizeTimestamps(batch)
			if err := w.sink.SaveEvents(ctx, batch); err != nil {
				return offset, fmt.Errorf("save events batch: %w", err)
			}
			offset += decoder.InputOffset()
			w.saveOffset(offset)
			batch = batch[:0]
		}
	}

	// Flush remaining batch
	if len(batch) > 0 {
		w.finalizeTimestamps(batch)
		if err := w.sink.SaveEvents(ctx, batch); err != nil {
			return offset, fmt.Errorf("save events final batch: %w", err)
		}
	}

	return offset + decoder.InputOffset(), nil
}

// finalizeTimestamps adjusts the event timestamps in a batch.
// Since CLH only gives uptime, we assume the last event in the batch happened "now"
// and calculate previous ones relative to it.
func (w *sandboxWatcher) finalizeTimestamps(batch []*model.SandboxEvent) {
	if len(batch) == 0 {
		return
	}

	now := time.Now()
	lastUptime := time.Duration(batch[len(batch)-1].Meta["uptime_ns"].(int64)) * time.Nanosecond

	for _, ev := range batch {
		uptime := time.Duration(ev.Meta["uptime_ns"].(int64)) * time.Nanosecond
		// eventTime = Now - (CurrentUptime - EventUptime)
		ev.Timestamp = now.Add(-(lastUptime - uptime))
	}
}

// -----------------------------------------------------------------------------
// EventMonitor — manages the lifecycle of all per-VM watchers
// -----------------------------------------------------------------------------

// EventMonitor starts, stops, and resumes watchers for all sandboxes.
type EventMonitor struct {
	mu       sync.Mutex
	watchers map[string]*sandboxWatcher // keyed by sandbox hex ID
	sink     EventSink
	cfg      WatcherConfig
	rootCtx  context.Context
}

// NewEventMonitor creates a monitor. Call ResumeAll once after server init.
func NewEventMonitor(sink EventSink) *EventMonitor {
	return &EventMonitor{
		watchers: make(map[string]*sandboxWatcher),
		sink:     sink,
		cfg:      defaultWatcherConfig(),
	}
}

// WithConfig replaces the default watcher config (optional).
func (m *EventMonitor) WithConfig(cfg WatcherConfig) *EventMonitor {
	m.cfg = cfg
	return m
}

// SetRootContext provides the parent context used by all watchers.
// Must be called before Start or ResumeAll.
func (m *EventMonitor) SetRootContext(ctx context.Context) {
	m.mu.Lock()
	m.rootCtx = ctx
	m.mu.Unlock()
}

// Start begins watching the CLH event file for a newly created sandbox.
func (m *EventMonitor) Start(ctx context.Context, sbxID primitive.ObjectID, orgID, userID primitive.ObjectID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := sbxID.Hex()
	if _, exists := m.watchers[id]; exists {
		return // already watching
	}

	parent := m.rootCtx
	if parent == nil {
		parent = ctx
	}

	w := newSandboxWatcher(SandboxMeta{
		SandboxID: sbxID,
		OrgID:     orgID,
		UserID:    userID,
	}, m.sink, m.cfg)
	w.start(parent)
	m.watchers[id] = w
}

// Stop cancels the watcher for the given sandbox (called on delete).
func (m *EventMonitor) Stop(ctx context.Context, sbxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if w, ok := m.watchers[sbxID]; ok {
		// Perform one last sync to capture final events (e.g. shutdown)
		offset := w.loadOffset()
		newOffset, err := w.poll(ctx, offset)
		if err == nil && newOffset != offset {
			w.saveOffset(newOffset)
		}

		w.stop()
		delete(m.watchers, sbxID)
	}
}

// ResumeAll re-attaches watchers to sandboxes that were active before a server restart.
// Safe to call at startup before any HTTP traffic.
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
		// Only resume if event file actually exists on disk
		if _, err := os.Stat(GetEventPath(id)); os.IsNotExist(err) {
			continue
		}
		w := newSandboxWatcher(meta, m.sink, m.cfg)
		w.start(parent)
		m.watchers[id] = w
		resumed++
	}

	if resumed > 0 {
		log.Printf("[event_monitor] resumed %d watcher(s) after server restart", resumed)
	}
}
