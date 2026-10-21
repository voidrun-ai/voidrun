package service

import (
	"context"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type createOrgKey struct{}

// WithCreateOrg stores the org that a sandbox create is billed to.
func WithCreateOrg(ctx context.Context, id primitive.ObjectID) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, createOrgKey{}, id)
}

// CreateOrgFromContext returns the org stored by WithCreateOrg.
func CreateOrgFromContext(ctx context.Context) (primitive.ObjectID, bool) {
	if ctx == nil {
		return primitive.NilObjectID, false
	}
	id, ok := ctx.Value(createOrgKey{}).(primitive.ObjectID)
	if !ok || id.IsZero() {
		return primitive.NilObjectID, false
	}
	return id, true
}

type createHeldKey struct{}

// WithCreateHeld marks a sandbox row that already counts toward the create gate.
func WithCreateHeld(ctx context.Context, id primitive.ObjectID) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, createHeldKey{}, id)
}

// CreateHeldFromContext returns the sandbox row stored by WithCreateHeld.
func CreateHeldFromContext(ctx context.Context) (primitive.ObjectID, bool) {
	if ctx == nil {
		return primitive.NilObjectID, false
	}
	id, ok := ctx.Value(createHeldKey{}).(primitive.ObjectID)
	if !ok || id.IsZero() {
		return primitive.NilObjectID, false
	}
	return id, true
}
