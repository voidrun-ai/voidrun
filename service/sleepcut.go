package service

import (
	"time"

	"voidrun/model"
)

type sleepAct int

const (
	sleepNone sleepAct = iota
	sleepPack
	sleepArchive
	sleepDelete
)

// dueSleepAction picks the one sleep-stage action that is due.
// Delete is checked before archive, then pack. A nil snapshottedAt is not due.
// Zero packAfterSec or archiveAfterSec skips that step.
func dueSleepAction(sb *model.Sandbox, deleteAfterSec, packAfterSec, archiveAfterSec int, hasStore bool, now time.Time) sleepAct {
	if sb == nil || sb.SnapshottedAt == nil {
		return sleepNone
	}
	deleteDue := remainingSince(sb.SnapshottedAt, deleteAfterSec, now) == 0
	switch sb.Status {
	case "snapshotted":
		if deleteDue {
			return sleepDelete
		}
		if sb.Packed && hasStore && remainingSince(sb.SnapshottedAt, archiveAfterSec, now) == 0 {
			return sleepArchive
		}
		if !sb.Packed && remainingSince(sb.SnapshottedAt, packAfterSec, now) == 0 {
			return sleepPack
		}
	case "archived":
		if deleteDue {
			return sleepDelete
		}
	}
	return sleepNone
}

// DuePackOrArchive reports "pack" or "archive" when that step is due.
// Delete stays on the server sweeper, so it returns "".
func DuePackOrArchive(sb *model.Sandbox, deleteAfterSec, packAfterSec, archiveAfterSec int, hasStore bool, now time.Time) string {
	switch dueSleepAction(sb, deleteAfterSec, packAfterSec, archiveAfterSec, hasStore, now) {
	case sleepPack:
		return "pack"
	case sleepArchive:
		return "archive"
	default:
		return ""
	}
}
