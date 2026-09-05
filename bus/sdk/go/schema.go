package sessionkit

import (
	"github.com/antst/agent-sessions/bus/internal/protocol"
)

type (
	ExtraArgument    = protocol.ExtraArgument
	HelloDescription = protocol.HelloDescription
	OpenOptions      = protocol.OpenOptions
	OpenRequest      = protocol.OpenRequest
	OpenResult       = protocol.OpenResult
	TurnResult       = protocol.TurnResult
	DeliverySource   = protocol.DeliverySource
	DeliveryRequest  = protocol.DeliveryRequest
	DeliveryReceipt  = protocol.DeliveryReceipt
	SessionSummary   = protocol.SessionSummary
	HostProducts     = protocol.HostProducts
	ProtocolError    = protocol.RPCError
)

// SessionSchema returns a private copy of the universal wire schema.
func SessionSchema() []byte { return append([]byte(nil), protocol.SessionSchema...) }
