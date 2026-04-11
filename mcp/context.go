package mcp

import (
	"context"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type contextKey string

const (
	orgIDKey  contextKey = "mcp_orgID"
	userIDKey contextKey = "mcp_userID"
)

// WithOrgID stores the org ID in the context.
func WithOrgID(ctx context.Context, id primitive.ObjectID) context.Context {
	return context.WithValue(ctx, orgIDKey, id)
}

// OrgIDFromContext retrieves the org ID from the context.
func OrgIDFromContext(ctx context.Context) (primitive.ObjectID, bool) {
	id, ok := ctx.Value(orgIDKey).(primitive.ObjectID)
	return id, ok
}

// WithUserID stores the user ID in the context.
func WithUserID(ctx context.Context, id primitive.ObjectID) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// UserIDFromContext retrieves the user ID from the context.
func UserIDFromContext(ctx context.Context) (primitive.ObjectID, bool) {
	id, ok := ctx.Value(userIDKey).(primitive.ObjectID)
	return id, ok
}
