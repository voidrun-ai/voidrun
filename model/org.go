package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Org struct {
	ID         primitive.ObjectID   `bson:"_id,omitempty" json:"id"`
	Name       string               `bson:"name" json:"name"`
	OwnerID    primitive.ObjectID   `bson:"ownerId" json:"ownerId"`
	Members    []primitive.ObjectID `bson:"members" json:"members"`
	Plan       string               `bson:"plan" json:"plan"`
	UsageCount float64              `bson:"usage" json:"usage"`
	Balance    float64              `bson:"balance" json:"balance"`
	// SandboxCreateRateLimit overrides the server default when greater than 0.
	SandboxCreateRateLimit int `bson:"sandboxCreateRateLimit,omitempty" json:"sandboxCreateRateLimit,omitempty"`
	// SandboxQuotaCount is the max number of non-deleted sandboxes. Overrides the server default when greater than 0.
	SandboxQuotaCount int `bson:"sandboxQuotaCount,omitempty" json:"sandboxQuotaCount,omitempty"`
	// SandboxQuotaCPU is the max total vCPU across those sandboxes.
	SandboxQuotaCPU int `bson:"sandboxQuotaCPU,omitempty" json:"sandboxQuotaCPU,omitempty"`
	// SandboxQuotaMemMB is the max total memory in MB across those sandboxes.
	SandboxQuotaMemMB int `bson:"sandboxQuotaMemMB,omitempty" json:"sandboxQuotaMemMB,omitempty"`

	CreatedAt time.Time          `json:"createdAt" bson:"createdAt"`
	CreatedBy primitive.ObjectID `json:"createdBy" bson:"createdBy"`
	UpdatedAt time.Time          `json:"updatedAt" bson:"updatedAt"`
}
