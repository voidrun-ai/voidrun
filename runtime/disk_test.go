package runtime

import "testing"

func TestSafePathRegexSandboxID(t *testing.T) {
	tests := []struct {
		id  string
		ok  bool
	}{
		{"a", true},
		{"sandbox-1", true},
		{"Ab_09", true},
		{strings64('a'), true},
		{"", false},
		{"has/slash", false},
		{"..", false},
		{"a.b", false},
		{"x y", false},
		{strings64('a') + "x", false},
	}
	for _, tt := range tests {
		if got := safePathRegex.MatchString(tt.id); got != tt.ok {
			t.Errorf("safePathRegex.MatchString(%q) = %v, want %v", tt.id, got, tt.ok)
		}
	}
}

func strings64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
