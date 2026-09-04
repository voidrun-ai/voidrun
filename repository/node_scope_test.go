package repository

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestHealthFilterScopesToNode(t *testing.T) {
	got := healthFilter("dcd-la-ovh-bhs-1.dcdeploy.com")
	if got["nodeId"] != "dcd-la-ovh-bhs-1.dcdeploy.com" {
		t.Fatalf("nodeId = %v, want host id", got["nodeId"])
	}
	status, ok := got["status"].(bson.M)
	if !ok {
		t.Fatalf("status filter type %T", got["status"])
	}
	if status["$ne"] != "deleted" {
		t.Fatalf("status $ne = %#v, want deleted only", status["$ne"])
	}
}

func TestHealthFilterEmptyNodeOmitsNodeKey(t *testing.T) {
	got := healthFilter("")
	if _, ok := got["nodeId"]; ok {
		t.Fatal("empty nodeID must not add nodeId filter")
	}
}

func TestFindForHealthEmptyNodeReturnsNil(t *testing.T) {
	r := &SandboxRepository{}
	got, err := r.FindForHealth(context.Background(), "", options.FindOptions{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Fatalf("got = %#v, want nil", got)
	}
}

func TestFindIdleRunningEmptyNodeReturnsNil(t *testing.T) {
	r := &SandboxRepository{}
	got, err := r.FindIdleRunning(context.Background(), "", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Fatalf("got = %#v, want nil", got)
	}
}

func TestIdleRunningFilterScopesToNode(t *testing.T) {
	got := idleRunningFilter("node-a", time.Unix(1, 0).UTC())
	if got["nodeId"] != "node-a" {
		t.Fatalf("nodeId = %v", got["nodeId"])
	}
	if got["status"] != "running" {
		t.Fatalf("status = %v", got["status"])
	}
}

func TestStaleSnapshottedFilterScopesToNode(t *testing.T) {
	got := staleSnapshottedFilter("node-b", time.Unix(1, 0).UTC())
	if got["nodeId"] != "node-b" {
		t.Fatalf("nodeId = %v", got["nodeId"])
	}
	if got["status"] != "snapshotted" {
		t.Fatalf("status = %v", got["status"])
	}
}

func TestAllocatedIPFilterEmptyNodeIsClusterWide(t *testing.T) {
	got := allocatedIPFilter("")
	if _, ok := got["nodeId"]; ok {
		t.Fatal("empty nodeID must not add nodeId")
	}
	if got["ip"].(bson.M)["$ne"] != "" {
		t.Fatal("must still require a non-empty ip")
	}
}

func TestAllocatedIPFilterScopesToNode(t *testing.T) {
	got := allocatedIPFilter("host-fra-01")
	if got["nodeId"] != "host-fra-01" {
		t.Fatalf("nodeId = %v", got["nodeId"])
	}
	if _, ok := got["$or"]; ok {
		t.Fatalf("must not add $or, got %#v", got["$or"])
	}
}
