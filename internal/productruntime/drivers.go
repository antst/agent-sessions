package productruntime

import (
	"context"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

type RuntimeProduct struct {
	Descriptor productcatalog.Descriptor
	Lane       LaneDriver
	Doctor     DoctorProbe
}

type LaneDriver interface {
	Capabilities() LaneCapabilitySet
	Open(context.Context, LaneOpenRequest) (NativeSessionRef, error)
	StartTurn(context.Context, NativeSessionRef, TurnStartRequest) (NativeTurnRef, error)
	WaitTurn(context.Context, NativeTurnRef) (NativeTerminal, error)
	Steer(context.Context, NativeTurnRef, TurnStartRequest) (NativeAcceptance, error)
	Interrupt(context.Context, NativeTurnRef) error
	Archive(context.Context, NativeSessionRef) error
}

type DoctorProbe interface {
	Probe(context.Context, ProbeRequest) (ProbeReport, error)
}
