package config

import "testing"

func TestHostIDFromEnv(t *testing.T) {
	t.Setenv("NODE_ID", "fra-01")
	t.Setenv("HOST_ID", "")
	if got := hostIDFromEnv(); got != "fra-01" {
		t.Fatalf("NODE_ID: got %q", got)
	}

	t.Setenv("NODE_ID", "")
	t.Setenv("HOST_ID", "legacy-host")
	if got := hostIDFromEnv(); got != "legacy-host" {
		t.Fatalf("HOST_ID: got %q", got)
	}

	t.Setenv("NODE_ID", "")
	t.Setenv("HOST_ID", "")
	if got := hostIDFromEnv(); got == "" {
		t.Fatal("expected hostname fallback")
	}
}
