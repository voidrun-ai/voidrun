package util

import "testing"

func TestValidateCreateSandboxRequest(t *testing.T) {
	if err := ValidateCreateSandboxRequest("valid-name", 2, 2048, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateCreateSandboxRequest("valid-name", 2, 2048, []int{80, 443}); err != nil {
		t.Fatalf("unexpected error with publishPorts: %v", err)
	}
	if err := ValidateCreateSandboxRequest("bad_name", 2, 2048, nil); err == nil {
		t.Fatal("expected invalid name error")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 0, 2048, nil).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for cpu")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 2, 512, nil).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for mem")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 2, 2048, []int{80, 81, 82, 83, 84}).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for too many publishPorts")
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
