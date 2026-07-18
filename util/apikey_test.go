package util

import (
	"strings"
	"testing"
)

func TestValidateAPIKeyName(t *testing.T) {
	if err := ValidateAPIKeyName(strings.Repeat("a", 25)); err != nil {
		t.Fatalf("25-character name should be valid: %v", err)
	}
	if err := ValidateAPIKeyName(strings.Repeat("a", 26)); err == nil {
		t.Fatal("expected error for name longer than 25 characters")
	}
	if err := ValidateAPIKeyName(strings.Repeat("🚀", 25)); err != nil {
		t.Fatalf("25-character Unicode name should be valid: %v", err)
	}
	if err := ValidateAPIKeyName("   "); err == nil {
		t.Fatal("expected error for blank name")
	}
}
