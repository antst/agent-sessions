package productruntime

import (
	"context"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

type RuntimeProduct struct {
	Descriptor        productcatalog.Descriptor
	Peer              PeerDriver
	Message           MessageDriver
	NativeTitle       NativeTitleProjector
	Lane              LaneDriver
	Parent            ParentAttester
	Doctor            DoctorProbe
	ComponentResolver ComponentResolver
	ComponentRebinder ComponentSessionRebinder
}

type PeerDriver interface {
	AttachmentAdapter(HostDeps) (daemon.AttachmentAdapter, error)
	BuildLaunch(context.Context, PeerLaunchRequest) (NativeCommand, error)
	Rename(context.Context, daemon.ManagedAttachment, string) (NativeName, error)
}

type MessageDriver interface {
	Deliver(context.Context, daemon.ManagedAttachment, DeliveryRequest) (NativeAcceptance, error)
}

// NativeTitleProjector reads the product-owned title of one exact live native
// session. A successful projection is a live observation; implementations may
// query the product directly or follow authenticated generation-local native
// observations, but must never treat a durable daemon copy as authority.
type NativeTitleProjector interface {
	ProjectNativeTitle(context.Context, daemon.ManagedAttachment) (NativeTitleProjection, error)
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
