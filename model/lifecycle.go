package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	OpCreate = "create"
	OpSleep  = "sleep"
	OpWake   = "wake"
	OpDelete = "delete"
	OpKill   = "kill"
	OpPorts  = "ports"
)

const (
	PhaseStarting  = "starting"
	PhaseCommitted = "committed"
	PhaseFailed    = "failed"
)

const EventSourceSupervisor = "supervisor"

// LifecycleEvent is a supervisor-owned sandbox transition. Subscribers must not
// treat this as a mutable *Sandbox pointer.
type LifecycleEvent struct {
	EventID      string
	SandboxID    string
	OrgID        string
	UserID       string
	Op           string
	Phase        string
	FromStatus   string
	ToStatus     string
	Generation   uint64
	Timestamp    time.Time
	SandboxName  string
	IP           string
	PublishPorts []int
}

func (e LifecycleEvent) EventName() string {
	if e.Op == "" || e.Phase == "" {
		return ""
	}
	return e.Op + "-" + e.Phase
}

func (e LifecycleEvent) ToSandboxEvent() *SandboxEvent {
	sid, _ := primitive.ObjectIDFromHex(e.SandboxID)
	oid, _ := primitive.ObjectIDFromHex(e.OrgID)
	uid, _ := primitive.ObjectIDFromHex(e.UserID)
	eid, _ := primitive.ObjectIDFromHex(e.EventID)
	meta := map[string]any{
		"op":         e.Op,
		"phase":      e.Phase,
		"from":       e.FromStatus,
		"to":         e.ToStatus,
		"generation": int64(e.Generation),
	}
	if e.SandboxName != "" {
		meta["name"] = e.SandboxName
	}
	if e.IP != "" {
		meta["ip"] = e.IP
	}
	if len(e.PublishPorts) > 0 {
		meta["publishPorts"] = e.PublishPorts
	}
	return &SandboxEvent{
		ID:        eid,
		SandboxID: sid,
		OrgID:     oid,
		UserID:    uid,
		Event:     e.EventName(),
		Source:    EventSourceSupervisor,
		Timestamp: e.Timestamp,
		Meta:      meta,
	}
}
