package service

import (
	"context"
	"testing"
	"time"

	"voidrun/config"
	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestLifecycleManagerFollowsHostID(t *testing.T) {
	id := "vr-ee-test-a"
	m := NewLifecycleManager(config.AutoLifecycleConfig{}, &id, nil, nil, nil, nil)
	if got := m.nodeID(); got != "vr-ee-test-a" {
		t.Fatalf("nodeID() = %q, want machine id", got)
	}

	id = "1538wb9aiu"
	if got := m.nodeID(); got != "1538wb9aiu" {
		t.Fatalf("nodeID() = %q after bind, want handle", got)
	}
}

func TestSweepSleepOnlyDeletes(t *testing.T) {
	id := "n1"
	m := NewLifecycleManager(config.AutoLifecycleConfig{
		DeleteAfterSnapshottedSec: 48 * 3600,
	}, &id, nil, nil, nil, nil)
	now := time.Unix(10_000, 0)
	past := now.Add(-time.Hour)
	sb := &model.Sandbox{Status: "snapshotted", SnapshottedAt: &past, Packed: true}
	if got := dueSleepAction(sb, m.cfg.DeleteAfterSnapshottedSec, 0, 0, true, now); got != sleepNone {
		t.Fatalf("archive = %v, want none", got)
	}
	sb.Packed = false
	if got := dueSleepAction(sb, m.cfg.DeleteAfterSnapshottedSec, 0, 0, true, now); got != sleepNone {
		t.Fatalf("pack = %v, want none", got)
	}
	old := now.Add(-72 * time.Hour)
	sb.Status = "archived"
	sb.SnapshottedAt = &old
	if got := dueSleepAction(sb, m.cfg.DeleteAfterSnapshottedSec, 0, 0, true, now); got != sleepDelete {
		t.Fatalf("delete = %v, want delete", got)
	}
}

func TestLifecycleManagerSweepNilRepo(t *testing.T) {
	m := NewLifecycleManager(config.AutoLifecycleConfig{Enabled: true}, nil, nil, nil, nil, &stubSnapshotter{})
	m.sweepSleep(context.Background())
}

type stubSnapshotter struct {
	calls int
}

func (s *stubSnapshotter) ApplySleep(context.Context, primitive.ObjectID, string) error {
	s.calls++
	return nil
}

func TestSleepJobTimeout(t *testing.T) {
	if sleepJobTimeout(sleepArchive) != time.Minute {
		t.Fatal("archive start timeout")
	}
	if sleepJobTimeout(sleepPack) != 30*time.Minute || sleepJobTimeout(sleepDelete) != 30*time.Minute {
		t.Fatal("pack and delete timeout")
	}
}
