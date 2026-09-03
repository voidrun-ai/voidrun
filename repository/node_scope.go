package repository

import (
	"context"
	"time"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func healthFilter(nodeID string) bson.M {
	// Include killed/error so this node can resurrect false-killed rows when the VM is still up.
	filter := bson.M{"status": bson.M{"$ne": "deleted"}}
	if nodeID != "" {
		filter["nodeId"] = nodeID
	}
	return filter
}

func idleRunningFilter(nodeID string, threshold time.Time) bson.M {
	filter := bson.M{
		"status": "running",
		"$or": []bson.M{
			{"autoSleep": bson.M{"$ne": false}},
			{"autoSleep": bson.M{"$exists": false}},
		},
		"lastActivityAt": bson.M{"$lt": threshold},
	}
	if nodeID != "" {
		filter["nodeId"] = nodeID
	}
	return filter
}

func staleSnapshottedFilter(nodeID string, threshold time.Time) bson.M {
	filter := bson.M{
		"status":        "snapshotted",
		"snapshottedAt": bson.M{"$lt": threshold},
	}
	if nodeID != "" {
		filter["nodeId"] = nodeID
	}
	return filter
}

// allocatedIPFilter selects IPs this node must not reissue. Empty nodeID
// keeps the historical cluster-wide scan (single-host / tests).
func allocatedIPFilter(nodeID string) bson.M {
	filter := bson.M{
		"ip":     bson.M{"$ne": ""},
		"status": bson.M{"$nin": []string{"deleted", "killed"}},
	}
	if nodeID != "" {
		filter["nodeId"] = nodeID
	}
	return filter
}

func (r *SandboxRepository) FindForHealth(ctx context.Context, nodeID string, opts options.FindOptions) ([]*model.Sandbox, error) {
	if nodeID == "" {
		return nil, nil
	}
	cursor, err := r.collection.Find(ctx, healthFilter(nodeID), &opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var sandboxes []*model.Sandbox
	if err = cursor.All(ctx, &sandboxes); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

// FindIdleRunning finds running sandboxes on this node that have been idle since before the threshold.
func (r *SandboxRepository) FindIdleRunning(ctx context.Context, nodeID string, threshold time.Time) ([]*model.Sandbox, error) {
	if nodeID == "" {
		return nil, nil
	}
	cursor, err := r.collection.Find(ctx, idleRunningFilter(nodeID, threshold), &options.FindOptions{
		Projection: bson.M{"_id": 1, "orgId": 1, "name": 1},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var sandboxes []*model.Sandbox
	if err = cursor.All(ctx, &sandboxes); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

// FindStaleSnapshotted finds snapshotted sandboxes on this node older than the threshold.
func (r *SandboxRepository) FindStaleSnapshotted(ctx context.Context, nodeID string, threshold time.Time) ([]*model.Sandbox, error) {
	if nodeID == "" {
		return nil, nil
	}
	cursor, err := r.collection.Find(ctx, staleSnapshottedFilter(nodeID, threshold), &options.FindOptions{
		Projection: bson.M{"_id": 1, "orgId": 1, "name": 1, "createdBy": 1, "tapName": 1, "netnsName": 1},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var sandboxes []*model.Sandbox
	if err = cursor.All(ctx, &sandboxes); err != nil {
		return nil, err
	}
	return sandboxes, nil
}
