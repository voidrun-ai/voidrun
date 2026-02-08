package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// APIKey represents an organization's API key with metadata
type APIKey struct {
	ID         primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	OrgID      primitive.ObjectID `bson:"orgId" json:"orgId"`
	Name       string             `bson:"name" json:"name"`
	Hash       string             `bson:"hash" json:"hash"` // Bcrypt hash - never expose
	CreatedBy  primitive.ObjectID `bson:"createdBy" json:"createdBy"`
	CreatedAt  time.Time          `bson:"createdAt" json:"createdAt"`
	LastUsedAt time.Time          `bson:"lastUsedAt,omitempty" json:"lastUsedAt,omitempty"`
	IsActive   bool               `bson:"isActive" json:"isActive"`
	UpdatedAt  time.Time          `bson:"updatedAt" json:"updatedAt"`
}

// APIKeyResponse represents the response when returning API key info (hash is omitted)
type APIKeyResponse struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"orgId"`
	Name       string    `json:"name"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	IsActive   bool      `json:"isActive"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// ToResponse converts APIKey to APIKeyResponse (excludes hash)
func (a *APIKey) ToResponse() APIKeyResponse {
	return APIKeyResponse{
		ID:         a.ID.Hex(),
		OrgID:      a.OrgID.Hex(),
		Name:       a.Name,
		CreatedBy:  a.CreatedBy.Hex(),
		CreatedAt:  a.CreatedAt,
		LastUsedAt: a.LastUsedAt,
		IsActive:   a.IsActive,
		UpdatedAt:  a.UpdatedAt,
	}
}
