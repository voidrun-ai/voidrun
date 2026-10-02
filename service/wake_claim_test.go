package service

import (
	"os"
	"path/filepath"
	"testing"

	"voidrun/model"
	"voidrun/runtime"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestLocalSnapshotReady(t *testing.T) {
	prev := runtime.InstancesRoot
	runtime.InstancesRoot = t.TempDir()
	t.Cleanup(func() { runtime.InstancesRoot = prev })

	id := primitive.NewObjectID().Hex()
	sb := &model.Sandbox{Status: "snapshotted"}
	if localSnapshotReady(sb, id) {
		t.Fatal("ready without a snapshot dir")
	}

	dir := filepath.Join(runtime.GetSnapshotBaseDir(id), "snap-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !localSnapshotReady(sb, id) {
		t.Fatal("snapshotted unpacked sandbox with a snapshot should skip the claim")
	}

	sb.Packed = true
	if localSnapshotReady(sb, id) {
		t.Fatal("packed sandbox must still claim")
	}
	sb.Packed = false
	sb.ColdCleared = true
	if localSnapshotReady(sb, id) {
		t.Fatal("cold-cleared sandbox must still claim")
	}
	sb.ColdCleared = false
	sb.Status = "archived"
	if localSnapshotReady(sb, id) {
		t.Fatal("archived sandbox must still claim")
	}
}
