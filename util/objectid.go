package util

import (
	"fmt"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ParseObjectID converts a hex string to a MongoDB ObjectID.
// Returns primitive.NilObjectID and an error if the string is invalid.
func ParseObjectID(id string) (primitive.ObjectID, error) {
	objID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return primitive.NilObjectID, fmt.Errorf("invalid object id format: %w", err)
	}
	return objID, nil
}

// IsValidObjectID returns true if the provided string is a valid ObjectID hex.
func IsValidObjectID(id string) bool {
	_, err := primitive.ObjectIDFromHex(id)
	return err == nil
}

// GenerateObjectID generates a new MongoDB ObjectID.
func GenerateObjectID() primitive.ObjectID {
	return primitive.NewObjectID()
}
