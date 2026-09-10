package util

import (
	"strings"
	"testing"
)

func TestDecodeStrictJSONRejectsUnknownField(t *testing.T) {
	t.Parallel()
	var dst struct {
		AutoSleep *bool `json:"autoSleep"`
	}
	err := DecodeStrictJSON(strings.NewReader(`{"cpu":2}`), &dst)
	if err == nil {
		t.Fatal("expected unknown field error")
	}
	if !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("got %q, want mention of cpu", err)
	}
}

func TestDecodeStrictJSONAcceptsKnownField(t *testing.T) {
	t.Parallel()
	var dst struct {
		AutoSleep *bool `json:"autoSleep"`
	}
	if err := DecodeStrictJSON(strings.NewReader(`{"autoSleep":false}`), &dst); err != nil {
		t.Fatal(err)
	}
	if dst.AutoSleep == nil || *dst.AutoSleep {
		t.Fatalf("got %#v, want false", dst.AutoSleep)
	}
}
