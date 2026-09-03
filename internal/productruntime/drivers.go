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

// LaneMessageDriver is implemented when a daemon-owned product session has a
// native inbound message path. Other lane drivers receive messages through
// their held presence connection.
type LaneMessageDriver interface {
	SendMessage(context.Context, NativeSessionRef, NativeMessage) error
}

type NativeMessageSource struct {
	UUID    string   `json:"uuid"`
	Name    string   `json:"name"`
	Product string   `json:"product"`
	Groups  []string `json:"groups"`
}

type NativeMessage struct {
	ID   string              `json:"message_id"`
	From NativeMessageSource `json:"from"`
	Body string              `json:"body"`
}

type DoctorProbe interface {
	Probe(context.Context, ProbeRequest) (ProbeReport, error)
}
