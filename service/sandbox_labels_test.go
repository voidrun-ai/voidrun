package service

import (
	"reflect"
	"testing"
	"time"

	"voidrun/config"
	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSandboxListFilterUsesLabelsDirectly(t *testing.T) {
	orgID := primitive.NewObjectID()
	got := sandboxListFilter(orgID, map[string]string{
		"env":  "prod",
		"team": "api",
	})
	want := bson.M{
		"orgId":       orgID,
		"labels.env":  "prod",
		"labels.team": "api",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected filter: got %#v, want %#v", got, want)
	}
	if _, exists := got["labelPairs"]; exists {
		t.Fatal("labelPairs must not be used for filtering")
	}
}

func TestDiskMBForCreateUsesImageSizeGB(t *testing.T) {
	t.Parallel()
	got := diskMBForCreate(5120, &model.Image{SizeGB: 10})
	if got != 10240 {
		t.Fatalf("got %d, want 10240", got)
	}
}

func TestDiskMBForCreateFallsBackToDefault(t *testing.T) {
	t.Parallel()
	if got := diskMBForCreate(5120, nil); got != 5120 {
		t.Fatalf("nil image: got %d, want 5120", got)
	}
	if got := diskMBForCreate(5120, &model.Image{SizeGB: 0}); got != 5120 {
		t.Fatalf("zero SizeGB: got %d, want 5120", got)
	}
}

func TestDiskMBForCreateDocker20G(t *testing.T) {
	t.Parallel()
	got := diskMBForCreate(10240, &model.Image{SizeGB: 20})
	if got != 20480 {
		t.Fatalf("got %d, want 20480", got)
	}
}

func TestSandboxSyncTimeout(t *testing.T) {
	t.Parallel()
	want := time.Duration(config.DefaultSandboxSyncTimeoutSec) * time.Second
	if got := sandboxSyncTimeout(0); got != want {
		t.Fatalf("zero: got %s, want %s", got, want)
	}
	if got := sandboxSyncTimeout(-1); got != want {
		t.Fatalf("negative: got %s, want %s", got, want)
	}
	if got := sandboxSyncTimeout(15); got != 15*time.Second {
		t.Fatalf("explicit: got %s, want 15s", got)
	}
}
