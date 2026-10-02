package service

import (
	"testing"
	"time"

	"voidrun/model"
)

func TestDueSleepActionOrder(t *testing.T) {
	now := time.Unix(10_000, 0)
	start := now.Add(-2 * time.Hour)
	sb := &model.Sandbox{Status: "snapshotted", SnapshottedAt: &start}
	if got := dueSleepAction(sb, 86400, 3600, 7200, true, now); got != sleepPack {
		t.Fatalf("unpacked = %v, want pack", got)
	}
	sb.Packed = true
	if got := dueSleepAction(sb, 86400, 3600, 7200, true, now); got != sleepArchive {
		t.Fatalf("packed = %v, want archive", got)
	}
	if got := dueSleepAction(sb, 86400, 3600, 7200, false, now); got != sleepNone {
		t.Fatalf("no store = %v, want none", got)
	}
	old := now.Add(-48 * time.Hour)
	sb.SnapshottedAt = &old
	if got := dueSleepAction(sb, 86400, 3600, 7200, true, now); got != sleepDelete {
		t.Fatalf("past delete = %v, want delete", got)
	}
	sb.Status = "archived"
	sb.Packed = false
	if got := dueSleepAction(sb, 86400, 3600, 7200, true, now); got != sleepDelete {
		t.Fatalf("archived = %v, want delete", got)
	}
}

func TestDueSleepActionNilTimestamp(t *testing.T) {
	now := time.Unix(10_000, 0)
	sb := &model.Sandbox{Status: "snapshotted"}
	if got := dueSleepAction(sb, 60, 60, 0, true, now); got != sleepNone {
		t.Fatalf("nil snapshottedAt = %v, want none", got)
	}
}

func TestDueSleepActionDisabledStage(t *testing.T) {
	now := time.Unix(10_000, 0)
	start := now.Add(-time.Hour)
	sb := &model.Sandbox{Status: "snapshotted", SnapshottedAt: &start}
	if got := dueSleepAction(sb, 0, 0, 0, false, now); got != sleepNone {
		t.Fatalf("zero config = %v, want none", got)
	}
}
