package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/statestore"
)

// DeliveryPresenter performs one product- or network-specific destination
// acceptance. deliveryID is stable across retries and must be used as the
// native delivery idempotency key.
type DeliveryPresenter func(
	context.Context,
	federation.Peer,
	federation.Peer,
	string,
	federation.AgentFrame,
) error

// DeliveryEngine owns group admission plus durable, metadata-only delivery
// acceptance. Message content is presented to adapters but never persisted in
// the daemon catalog.
type DeliveryEngine struct {
	store   *StateStore
	present DeliveryPresenter
	now     func() time.Time
	mu      sync.Mutex
}

// NewDeliveryEngine constructs the daemon-owned local delivery engine.
func NewDeliveryEngine(store *StateStore, present DeliveryPresenter) (*DeliveryEngine, error) {
	if store == nil || present == nil {
		return nil, errors.New("delivery engine requires state and presenter")
	}
	return &DeliveryEngine{store: store, present: present, now: time.Now}, nil
}

// Route admits one discover/send/broadcast frame and completes each
// destination through a stable delivery record. A returned accepted result
// means the destination callback accepted it, never merely that it was queued.
func (e *DeliveryEngine) Route(
	ctx context.Context,
	frame federation.AgentFrame,
	source federation.Peer,
	peers []federation.Peer,
) (federation.AgentFrameResult, error) {
	return e.route(ctx, frame, source, peers, nil)
}

// RouteWithAcknowledgedPresentation re-presents only selected already-
// acknowledged destinations with the same stable delivery ID. This supports
// destination-owned receipt re-query without changing or duplicating the
// durable source delivery record.
func (e *DeliveryEngine) RouteWithAcknowledgedPresentation(
	ctx context.Context,
	frame federation.AgentFrame,
	source federation.Peer,
	peers []federation.Peer,
	represent func(federation.Peer) bool,
) (federation.AgentFrameResult, error) {
	return e.route(ctx, frame, source, peers, represent)
}

func (e *DeliveryEngine) route(
	ctx context.Context,
	frame federation.AgentFrame,
	source federation.Peer,
	peers []federation.Peer,
	represent func(federation.Peer) bool,
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
		deliveryResult := federation.DeliveryResult{
			Target: target.ID, SessionID: target.SessionID, DeliveryID: deliveryID, Status: "accepted",
		}
		representAcknowledged := represent != nil && represent(target)
		if err := e.presentOne(ctx, deliveryID, frame, delivered, admission.Source, target, representAcknowledged); err != nil {
			deliveryResult.Status = "failed"
			deliveryResult.Error = "destination did not accept the delivery"
			result.Deliveries = append(result.Deliveries, deliveryResult)
			continue
		}
		result.Deliveries = append(result.Deliveries, deliveryResult)
	}
	return result, nil
}

func (e *DeliveryEngine) presentOne(
	ctx context.Context,
	deliveryID string,
	request, delivered federation.AgentFrame,
	source, target federation.Peer,
	representAcknowledged bool,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	record, err := e.prepare(deliveryID, request, source, target)
	if err != nil {
		return err
	}
	switch record.State {
	case "acknowledged":
		if representAcknowledged {
			return e.present(ctx, source, target, deliveryID, delivered)
		}
		return nil
	case "presented":
		return e.transition(deliveryID, "acknowledged", "destination-accepted", "")
	case "prepared", "retryable":
		if err := e.transition(deliveryID, "accepted", "", ""); err != nil {
			return err
		}
	case "accepted":
		// A predecessor may have crashed after acceptance. Re-present with the
		// same stable delivery id so the product adapter can deduplicate.
	default:
		return fmt.Errorf("delivery %s is terminal in state %s", deliveryID, record.State)
	}
	if err := e.present(ctx, source, target, deliveryID, delivered); err != nil {
		commitErr := e.transition(deliveryID, "retryable", "", "presentation-failed")
		return errors.Join(err, commitErr)
	}
	if err := e.transition(deliveryID, "presented", "", ""); err != nil {
		return err
	}
	return e.transition(deliveryID, "acknowledged", "destination-accepted", "")
}

func (e *DeliveryEngine) prepare(
	id string,
	frame federation.AgentFrame,
	source, target federation.Peer,
) (Delivery, error) {
	var result Delivery
	err := e.mutate(func(catalog *Catalog) error {
		if existing, ok := catalog.Deliveries[id]; ok {
			if existing.CorrelationID != frame.MessageID || existing.Sender != source.ID ||
				len(existing.Destinations) != 1 || existing.Destinations[0] != target.ID ||
				existing.HostSuffix != target.HostID {
				return errors.New("delivery identity was reused for a different route")
			}
			result = existing
			return nil
		}
		record := Delivery{
			ID: id, CorrelationID: frame.MessageID, Sender: source.ID,
			Destinations: []string{target.ID}, HostSuffix: target.HostID,
			SentAt: e.now().UTC().UnixMilli(), State: "prepared",
		}
		if frame.Group != "" {
			record.Groups = []string{frame.Group}
		}
		catalog.Deliveries[id] = record
		catalog.Host.DeliveryRevision++
		result = record
		return nil
	})
	return result, err
}

func (e *DeliveryEngine) transition(id, state, acknowledgment, retryCause string) error {
	return e.mutate(func(catalog *Catalog) error {
		record, ok := catalog.Deliveries[id]
		if !ok {
			return errors.New("delivery record disappeared")
		}
		if record.State != state && !ValidLifecycleTransition("delivery", record.State, state) {
			return fmt.Errorf("invalid delivery transition %s -> %s", record.State, state)
		}
		record.State = state
		record.Acknowledgment = acknowledgment
		record.RetryCause = retryCause
		catalog.Deliveries[id] = record
		catalog.Host.DeliveryRevision++
		return nil
	})
}

func (e *DeliveryEngine) mutate(apply func(*Catalog) error) error {
	for attempt := 0; attempt < 8; attempt++ {
		snapshot, err := e.store.Read()
		if err != nil {
			return err
		}
		catalog := snapshot.Catalog
		if err := apply(&catalog); err != nil {
			return err
		}
		if _, err := e.store.Commit(snapshot.Revision, catalog); err != nil {
			if errors.Is(err, statestore.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return errors.New("delivery state changed too frequently")
}

func stableDeliveryID(messageID, targetID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(messageID) + "\x00" + strings.TrimSpace(targetID)))
	return "delivery-" + hex.EncodeToString(digest[:16])
}
