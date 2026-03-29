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

	CreatedAt time.Time          `json:"createdAt" bson:"createdAt"`
	CreatedBy primitive.ObjectID `json:"createdBy" bson:"createdBy"`
	UpdatedAt time.Time          `json:"updatedAt" bson:"updatedAt"`
}
