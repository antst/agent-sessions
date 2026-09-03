package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestRouteDeliveryUsesOnlyLiveDestinationAcceptance(t *testing.T) {
	source, peers := deliveryTestPeers()
	accepted := []string{}
	present := func(
		_ context.Context,
		gotSource, target federation.Peer,
		deliveryID string,
		frame federation.AgentFrame,
	) error {
		if gotSource.ID != source.ID || frame.Type != "delivery" || frame.Content != "hello" || deliveryID == "" {
			t.Fatalf("presentation = source=%+v target=%+v id=%q frame=%+v", gotSource, target, deliveryID, frame)
		}
		accepted = append(accepted, target.ID)
		return nil
	}
	discovered, err := RouteDelivery(context.Background(), federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "discover", MessageID: "discover-1",
	}, source, peers, present)
	if err != nil || len(discovered.Peers) != 2 || len(accepted) != 0 {
		t.Fatalf("discover = %+v accepted=%v err=%v", discovered, accepted, err)
	}
	multicast, err := RouteDelivery(context.Background(), federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "send-1",
		Targets: []string{"a", "reader-b"}, Content: "hello",
	}, source, peers, present)
	if err != nil || len(multicast.Deliveries) != 2 || multicast.Deliveries[0].Status != "accepted" || multicast.Deliveries[1].Status != "accepted" {
		t.Fatalf("multicast = %+v, %v", multicast, err)
	}
	broadcast, err := RouteDelivery(context.Background(), federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "broadcast", MessageID: "broadcast-1",
		Group: "team", Content: "hello",
	}, source, peers, present)
	if err != nil || len(broadcast.Deliveries) != 2 || len(accepted) != 4 {
		t.Fatalf("broadcast = %+v accepted=%v err=%v", broadcast, accepted, err)
	}
}

func TestRouteDeliveryReturnsTruthfulFailureAndCallerRetryRepresents(t *testing.T) {
	source, peers := deliveryTestPeers()
	var calls []string
	fail := true
	rejection := errors.New("recipient unavailable")
	present := func(_ context.Context, _, _ federation.Peer, deliveryID string, _ federation.AgentFrame) error {
		calls = append(calls, deliveryID)
		if fail {
			return rejection
		}
		return nil
	}
	frame := federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "retry-1",
		Targets: []string{"a"}, Content: "sender retains this body",
	}
	first, err := RouteDelivery(context.Background(), frame, source, peers, present)
	if err != nil || len(first.Deliveries) != 1 || first.Deliveries[0].Status != "failed" ||
		first.Deliveries[0].Error != rejection.Error() || !errors.Is(first.Deliveries[0].Cause, rejection) {
		t.Fatalf("first = %+v, %v", first, err)
	}
	fail = false
	second, err := RouteDelivery(context.Background(), frame, source, peers, present)
	if err != nil || second.Deliveries[0].Status != "accepted" {
		t.Fatalf("retry = %+v, %v", second, err)
	}
	if len(calls) != 2 || calls[0] != calls[1] {
		t.Fatalf("request-local product idempotency IDs = %v", calls)
	}
}

func deliveryTestPeers() (federation.Peer, []federation.Peer) {
	source := federation.Peer{ID: "source", SessionID: "source", Name: "writer", Groups: []string{"team"}}
	return source, []federation.Peer{
		{ID: "a", SessionID: "a", Name: "reader-a", Groups: []string{"team"}},
		{ID: "b", SessionID: "b", Name: "reader-b", Groups: []string{"team", "other"}},
		{ID: "hidden", SessionID: "hidden", Name: "reader-a", Groups: []string{"secret"}},
	}
}
