package util

import (
	"fmt"
	"testing"
)

func TestValidateCreateSandboxRequest(t *testing.T) {
	if err := ValidateCreateSandboxRequest("valid-name", 2, 2048, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateCreateSandboxRequest("valid-name", 2, 2048, []int{80, 443}, nil); err != nil {
		t.Fatalf("unexpected error with publishPorts: %v", err)
	}
	if err := ValidateCreateSandboxRequest("valid-name", 2, 2048, nil, map[string]string{"env": "prod", "team": ""}); err != nil {
		t.Fatalf("unexpected error with labels: %v", err)
	}
	if err := ValidateCreateSandboxRequest("bad_name", 2, 2048, nil, nil); err == nil {
		t.Fatal("expected invalid name error")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 0, 2048, nil, nil).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for cpu")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 2, 512, nil, nil).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for mem")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 2, 2048, []int{80, 81, 82, 83, 84}, nil).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for too many publishPorts")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 2, 2048, nil, map[string]string{"Bad Key": "x"}).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for invalid label key")
	}
}

func TestValidateLabels(t *testing.T) {
	if err := ValidateLabels(nil); err != nil {
		t.Fatalf("nil labels should be valid: %v", err)
	}
	if err := ValidateLabels(map[string]string{"env": "prod", "team": "backend-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateLabels(map[string]string{"env-1": "prod", "env_2": "dev"}); err != nil {
		t.Fatalf("dash and underscore should be valid in label keys: %v", err)
	}
	if err := ValidateLabels(map[string]string{"env.x": "prod"}); err == nil {
		t.Fatal("expected error for '.' in label key")
	}
	if err := ValidateLabels(map[string]string{"abcdefghijklmnopqrst": "01234567890123456789"}); err != nil {
		t.Fatalf("boundary lengths should be valid: %v", err)
	}
	if err := ValidateLabels(map[string]string{"key": "éééééééééééééééééééé"}); err != nil {
		t.Fatalf("20-character Unicode value should be valid: %v", err)
	}
	if err := ValidateLabels(map[string]string{"abcdefghijklmnopqrstu": "valid"}); err == nil {
		t.Fatal("expected error for label key longer than 20 characters")
	}
	if err := ValidateLabels(map[string]string{"key": "012345678901234567890"}); err == nil {
		t.Fatal("expected error for label value longer than 20 characters")
	}
	if err := ValidateLabels(map[string]string{"key": "ééééééééééééééééééééé"}); err == nil {
		t.Fatal("expected error for Unicode label value longer than 20 characters")
	}
	if err := ValidateLabels(map[string]string{"env": "pro,d"}); err == nil {
		t.Fatal("expected error for comma in value")
	}
	if err := ValidateLabels(map[string]string{"env=x": "prod"}); err == nil {
		t.Fatal("expected error for '=' in key")
	}
	if err := ValidateLabels(map[string]string{"-env": "prod"}); err == nil {
		t.Fatal("expected error for key not starting with alphanumeric")
	}

	fiveLabels := make(map[string]string, 5)
	for i := 0; i < 5; i++ {
		fiveLabels[fmt.Sprintf("k%d", i)] = "v"
	}
	if err := ValidateLabels(fiveLabels); err != nil {
		t.Fatalf("five labels should be valid: %v", err)
	}

	tooMany := make(map[string]string, 6)
	for i := 0; i < 6; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	if err := ValidateLabels(tooMany); err == nil {
		t.Fatal("expected error for too many labels")
	}
}

func TestParseLabelSelector(t *testing.T) {
	labels, err := ParseLabelSelector("")
	if err != nil || labels != nil {
		t.Fatalf("expected nil,nil for empty selector, got %v, %v", labels, err)
	}

	labels, err = ParseLabelSelector("env=prod,team=backend")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if labels["env"] != "prod" || labels["team"] != "backend" {
		t.Fatalf("unexpected parsed labels: %v", labels)
	}

	if _, err := ParseLabelSelector("env"); err == nil {
		t.Fatal("expected error for missing '='")
	}
}

func TestValidatePublishPorts(t *testing.T) {
	cases := []struct {
		name    string
		ports   []int
		wantErr bool
	}{
		{"nil is ok", nil, false},
		{"empty is ok", []int{}, false},
		{"one port", []int{80}, false},
		{"max allowed", []int{80, 443, 8080, 9000}, false},
		{"one over max", []int{80, 443, 8080, 9000, 9001}, true},
		{"port zero", []int{0}, true},
		{"port negative", []int{-1}, true},
		{"port too high", []int{70000}, true},
		{"duplicate", []int{80, 443, 80}, true},
		{"boundary: 1 and 65535", []int{1, 65535}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePublishPorts(tc.ports)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr {
				if _, ok := err.(*InvalidSandboxRequestError); !ok {
					t.Fatalf("expected *InvalidSandboxRequestError, got %T: %v", err, err)
				}
			}
		})
	}
}
