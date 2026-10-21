package service

import (
	"errors"

	"voidrun/service/supervisor"
)

type (
	Supervisor         = supervisor.Supervisor
	SupervisorRegistry = supervisor.SupervisorRegistry
	Admission          = supervisor.Admission
	LifecycleListener  = supervisor.LifecycleListener
)

var (
	ErrAdmissionDenied       = supervisor.ErrAdmissionDenied
	ErrRateLimited           = errors.New("sandbox create rate limit exceeded")
	ErrInsufficientBalance   = errors.New("organization balance is too low to create a sandbox")
	ErrResumeBalance         = errors.New("organization balance is too low to resume a sandbox")
	ErrQuotaExceeded         = errors.New("organization sandbox quota exceeded")
	ErrCreateGateUnavailable = errors.New("sandbox create check unavailable")
	errSupervisorStopped     = supervisor.ErrSupervisorStopped
)

func NewSupervisorRegistry() *SupervisorRegistry {
	return supervisor.NewSupervisorRegistry()
}
