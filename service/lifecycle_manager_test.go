package service

import (
	"testing"

	"voidrun/config"
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
