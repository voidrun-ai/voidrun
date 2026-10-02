package repository

import (
	"context"

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

func sleepingFilter(nodeID string) bson.M {
	filter := bson.M{"status": bson.M{"$in": []string{"snapshotted", "archived"}}}
	if nodeID != "" {
		filter["nodeId"] = nodeID
	}
	return filter
}

// FindSleeping returns this node's snapshotted and archived sandboxes.
func (r *SandboxRepository) FindSleeping(ctx context.Context, nodeID string) ([]*model.Sandbox, error) {
	if nodeID == "" {
		return nil, nil
	}
	cursor, err := r.collection.Find(ctx, sleepingFilter(nodeID), &options.FindOptions{
		Projection: bson.M{"_id": 1, "orgId": 1, "name": 1, "status": 1, "packed": 1, "snapshottedAt": 1, "archiveKey": 1, "coldCleared": 1},
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
