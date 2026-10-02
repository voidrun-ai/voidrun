package service

import "voidrun/service/supervisor"

type (
	Supervisor         = supervisor.Supervisor
	SupervisorRegistry = supervisor.SupervisorRegistry
	Admission          = supervisor.Admission
	LifecycleListener  = supervisor.LifecycleListener
)

var (
	ErrAdmissionDenied   = supervisor.ErrAdmissionDenied
	errSupervisorStopped = supervisor.ErrSupervisorStopped
)

func NewSupervisorRegistry() *SupervisorRegistry {
	return supervisor.NewSupervisorRegistry()
}
