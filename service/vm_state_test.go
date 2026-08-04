package service

import "testing"

func TestIsRunningVMState(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Running", true},
		{"RunningVirtualized", true},
		{"running", true},
		{"Paused", false},
		{"Shutdown", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isRunningVMState(tc.in); got != tc.want {
			t.Fatalf("isRunningVMState(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
