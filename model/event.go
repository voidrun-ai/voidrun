package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// SandboxEvent represents a lifecycle event for a sandbox
type SandboxEvent struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SandboxID primitive.ObjectID `bson:"sandboxId" json:"sandboxId"`
	OrgID     primitive.ObjectID `bson:"orgId" json:"orgId"`
	UserID    primitive.ObjectID `bson:"userId,omitempty" json:"userId,omitempty"`
	Event     string             `bson:"event" json:"event"`   // "booted", "shutdown", "paused", "resumed", "created", "deleted"
	Source    string             `bson:"source" json:"source"` // "clh", "api", "health-check"
	Timestamp time.Time          `bson:"timestamp" json:"timestamp"`
	Meta      map[string]any     `bson:"meta,omitempty" json:"meta,omitempty"`
}
