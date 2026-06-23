package cloudhypervisor

import (
	"testing"

	"voidrun/pkg/compute"
)

func TestNormalizeState(t *testing.T) {
	cases := map[string]compute.VMState{
		"Running":            compute.VMStateRunning,
		"Paused":             compute.VMStatePaused,
		"Shutdown":           compute.VMStateStopped,
		"Loaded":             compute.VMStateStopped,
		"runningvirtualized": compute.VMStateRunning,
	}
	for in, want := range cases {
		if got := normalizeState(in); got != want {
			t.Fatalf("%s: got %q want %q", in, got, want)
		}
	}
}
