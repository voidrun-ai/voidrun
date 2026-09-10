package service

import (
	"testing"

	"voidrun/model"
)

func TestSandboxUpdateSetAutoSleepFalse(t *testing.T) {
	t.Parallel()
	off := false
	got := sandboxUpdateSet(model.UpdateSandboxRequest{AutoSleep: &off})
	v, ok := got["autoSleep"]
	if !ok {
		t.Fatal("missing autoSleep")
	}
	b, ok := v.(bool)
	if !ok || b {
		t.Fatalf("autoSleep = %#v, want false", v)
	}
	if _, has := got["updatedAt"]; has {
		t.Fatal("updatedAt belongs on the repository $set, not the field map")
	}
}

func TestSandboxUpdateSetOmitsUnsetAutoSleep(t *testing.T) {
	t.Parallel()
	got := sandboxUpdateSet(model.UpdateSandboxRequest{})
	if _, ok := got["autoSleep"]; ok {
		t.Fatal("unset autoSleep must not appear in $set")
	}
}
