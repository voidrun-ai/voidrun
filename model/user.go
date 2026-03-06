package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type User struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Name      string             `bson:"name" json:"name"`
	Email     string             `bson:"email" json:"email"`
	OrgID     primitive.ObjectID `bson:"orgId,omitempty" json:"orgId,omitempty"`
	System    bool               `bson:"system" json:"system"`
	CreatedAt time.Time          `bson:"createdAt" json:"createdAt"`
	CreatedBy primitive.ObjectID `bson:"createdBy" json:"createdBy"`

	ClerkID  string `bson:"clerkId" json:"clerkId"`
	ImageURL string `bson:"imageUrl" json:"imageUrl"`
}
