package util

import "testing"

func TestValidateCreateSandboxRequest(t *testing.T) {
	if err := ValidateCreateSandboxRequest("valid-name", 2, 2048); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateCreateSandboxRequest("bad_name", 2, 2048); err == nil {
		t.Fatal("expected invalid name error")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 0, 2048).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for cpu")
	}
	if _, ok := ValidateCreateSandboxRequest("valid", 2, 512).(*InvalidSandboxRequestError); !ok {
		t.Fatal("expected InvalidSandboxRequestError for mem")
	}
}
