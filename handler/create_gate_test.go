package handler

import (
	"errors"
	"net/http"
	"testing"

	"voidrun/service"
	"voidrun/util"
)

func TestSandboxCreateError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "rate limit", err: service.ErrRateLimited, status: http.StatusTooManyRequests},
		{name: "balance", err: service.ErrInsufficientBalance, status: http.StatusPaymentRequired},
		{name: "quota", err: service.ErrQuotaExceeded, status: http.StatusForbidden},
		{name: "gate down", err: service.ErrCreateGateUnavailable, status: http.StatusServiceUnavailable},
		{name: "capacity", err: service.ErrAdmissionDenied, status: util.StatusOverloaded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sandboxCreateError(tt.err)
			var api *util.APIError
			if !errors.As(got, &api) {
				t.Fatalf("got %T", got)
			}
			if api.Status != tt.status {
				t.Fatalf("status=%d want %d", api.Status, tt.status)
			}
		})
	}
	if sandboxCreateError(errors.New("other")) != nil {
		t.Fatal("unknown error should pass through")
	}
}

func TestResumeDenied(t *testing.T) {
	got := resumeDenied(service.ErrResumeBalance)
	var api *util.APIError
	if !errors.As(got, &api) || api.Status != http.StatusPaymentRequired {
		t.Fatalf("resume balance: %#v", got)
	}
	if resumeDenied(errors.New("other")) != nil {
		t.Fatal("unknown error should pass through")
	}
}
