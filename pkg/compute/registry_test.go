package compute

import (
	"context"
	"encoding/json"
	"testing"
)

type stubHV struct{ name string }

func (s *stubHV) Name() string { return s.name }
func (s *stubHV) StartVM(ctx context.Context, cfg VMConfig) error { return nil }
func (s *stubHV) StopVM(ctx context.Context, id string) error       { return nil }
func (s *stubHV) StartGuest(ctx context.Context, id string) error  { return nil }
func (s *stubHV) PauseVM(ctx context.Context, id string) error     { return nil }
func (s *stubHV) ResumeVM(ctx context.Context, id string) error    { return nil }
func (s *stubHV) DeleteVM(ctx context.Context, id string) error    { return nil }
func (s *stubHV) Snapshot(ctx context.Context, id, dir string) error {
	return nil
}
func (s *stubHV) Restore(ctx context.Context, cfg VMConfig) error { return nil }
func (s *stubHV) GetState(ctx context.Context, id string) (VMState, error) {
	return VMStateRunning, nil
}
func (s *stubHV) Info(ctx context.Context, id string) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (s *stubHV) IsAvailable(id string) bool { return true }
func (s *stubHV) Counters(ctx context.Context, id string) (json.RawMessage, error) {
	return nil, nil
}

func TestGetUnknown(t *testing.T) {
	if _, err := Get("does_not_exist_hv"); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveType(t *testing.T) {
	if got := ResolveType("", ""); got != TypeCloudHypervisor {
		t.Fatalf("got %q", got)
	}
	if got := ResolveType("firecracker", "cloud_hypervisor"); got != TypeFirecracker {
		t.Fatalf("got %q", got)
	}
}
