package generic

import "go.mongodb.org/mongo-driver/bson/primitive"

// Entity is an interface that all your models must implement
type Entity interface {
	GetID() primitive.ObjectID
	SetID(primitive.ObjectID)
}
