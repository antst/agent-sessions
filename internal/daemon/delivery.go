package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/antst/sessionbus/internal/federation"
)

// DeliveryPresenter performs one synchronous product- or network-specific
// destination acceptance. deliveryID is request-local correlation for the
// recipient's native idempotency boundary; the daemon never stores it.
type DeliveryPresenter func(
	context.Context,
	federation.Peer,
	federation.Peer,
	string,
	federation.AgentFrame,
) error

// RouteDelivery admits one live discover/send frame and presents it
// directly to every currently live destination. A returned accepted result is
// the recipient carrier's acceptance. The daemon never queues, retries, or
// records the message itself.
func RouteDelivery(
	ctx context.Context,
	frame federation.AgentFrame,
	source federation.Peer,
	peers []federation.Peer,
	present DeliveryPresenter,
) (federation.AgentFrameResult, error) {
	admission, err := federation.Admit(frame, source, peers)
	if err != nil {
		return federation.AgentFrameResult{}, err
	}
	result := federation.AgentFrameResult{Version: federation.AgentFrameVersion, MessageID: frame.MessageID}
	if frame.Type == "discover" {
		result.Type = "discover.result"
		result.Peers = append([]federation.Peer(nil), admission.Targets...)
		return result, nil
	}
	result.Type = frame.Type + ".result"
	delivered := federation.DeliveryFrame(frame, admission.Source)
	result.Deliveries = make([]federation.DeliveryResult, 0, len(admission.Targets))
	for _, target := range admission.Targets {
		deliveryID := stableDeliveryID(frame.MessageID, target.ID)
		outcome := federation.DeliveryResult{
			Target: target.ID, SessionID: target.SessionID, DeliveryID: deliveryID, Status: "accepted",
		}
		var presentErr error
		if present == nil {
			presentErr = errors.New("destination has no delivery path")
		} else {
			presentErr = present(ctx, admission.Source, target, deliveryID, delivered)
		}
		if presentErr != nil {
			outcome.Status = "failed"
			outcome.Error = presentErr.Error()
			outcome.Cause = presentErr
		}
		result.Deliveries = append(result.Deliveries, outcome)
	}
	return result, nil
}

func stableDeliveryID(messageID, targetID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(messageID) + "\x00" + strings.TrimSpace(targetID)))
	return "delivery-" + hex.EncodeToString(digest[:16])
}
