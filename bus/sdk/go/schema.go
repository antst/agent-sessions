package sessionkit

import (
	"github.com/antst/agent-sessions/bus/internal/livepresence"
	"github.com/antst/agent-sessions/bus/internal/protocol"
)

type (
	ExtraArgument    = livepresence.SessionExtraArgument
	HelloDescription = livepresence.SessionHello
	OpenOptions      = livepresence.SessionOpenOptions
	OpenRequest      = livepresence.SessionOpenRequest
	OpenResult       = livepresence.SessionOpenResult
	TurnResult       = livepresence.SessionTurnResult
	DeliverySource   = livepresence.SessionDeliverySource
	DeliveryRequest  = livepresence.SessionDeliveryRequest
	DeliveryReceipt  = livepresence.SessionDeliveryReceipt
	ProtocolError    = livepresence.RPCError
)

// SessionSchema returns a private copy of the universal wire schema.
func SessionSchema() []byte { return append([]byte(nil), protocol.SessionSchema...) }
