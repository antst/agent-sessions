package productruntime

import (
	"context"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

type RuntimeProduct struct {
	Descriptor productcatalog.Descriptor
	Peer       PeerDriver
	Message    MessageDriver
	Lane       LaneDriver
	Parent     ParentAttester
	Doctor     DoctorProbe
}

type PeerDriver interface {
	AttachmentAdapter(HostDeps) (daemon.AttachmentAdapter, error)
	BuildLaunch(context.Context, PeerLaunchRequest) (NativeCommand, error)
	Rename(context.Context, daemon.ManagedAttachment, string) (NativeName, error)
}

type MessageDriver interface {
	Deliver(context.Context, daemon.ManagedAttachment, DeliveryRequest) (NativeAcceptance, error)
}

type LaneDriver interface {
	Capabilities() LaneCapabilitySet
	Open(context.Context, LaneOpenRequest) (NativeSessionRef, error)
	StartTurn(context.Context, NativeSessionRef, TurnStartRequest) (NativeTurnRef, error)
	WaitTurn(context.Context, NativeTurnRef) (NativeTerminal, error)
	Steer(context.Context, NativeTurnRef, TurnStartRequest) (NativeAcceptance, error)
	Interrupt(context.Context, NativeTurnRef) error
	Archive(context.Context, NativeSessionRef) error
	Recover(context.Context, LaneRecoveryRequest) (NativeSessionRef, error)
}

type ParentAttester interface {
	Attest(context.Context, ConnectorAttempt) (ParentBinding, error)
}

type DoctorProbe interface {
	Probe(context.Context, ProbeRequest) (ProbeReport, error)
}
