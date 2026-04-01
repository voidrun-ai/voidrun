package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Image represents a base image for creating sandboxes
type Image struct {
	ID        primitive.ObjectID `json:"_id" bson:"_id"`
	Name      string             `json:"name" bson:"name"`
	Tag       string             `json:"tag" bson:"tag"`
	System    bool               `json:"system" bson:"system"`
	SizeGB    int64              `json:"sizeGB" bson:"sizeGB"`
	Active    bool               `json:"active" bson:"active"`
	OrgID       primitive.ObjectID `json:"orgId" bson:"orgId"`
	Description string             `json:"description" bson:"description"`
	CreatedAt   time.Time          `json:"createdAt" bson:"createdAt"`
	CreatedBy primitive.ObjectID `json:"createdBy" bson:"createdBy"`
}
