package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestDeliveryEngineRoutesDiscoverMulticastAndBroadcastWithDestinationAcceptance(t *testing.T) {
	store := openDeliveryTestState(t)
	var mu sync.Mutex
	accepted := []string{}
	engine, err := NewDeliveryEngine(store, func(
		_ context.Context,
		source, target federation.Peer,
		deliveryID string,
		frame federation.AgentFrame,
	) error {
		if source.ID != "source" || frame.Type != "delivery" || frame.Content != "hello" || deliveryID == "" {
			t.Fatalf("presentation = source=%+v target=%+v id=%q frame=%+v", source, target, deliveryID, frame)
		}
		mu.Lock()
		accepted = append(accepted, target.ID)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	source, peers := deliveryTestPeers()
	discovered, err := engine.Route(context.Background(), federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "discover", MessageID: "discover-1",
	}, source, peers)
	if err != nil || len(discovered.Peers) != 2 {
		t.Fatalf("discover = %+v, %v", discovered, err)
	}
	multicast, err := engine.Route(context.Background(), federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "send-1",
		Targets: []string{"a", "reader-b"}, Content: "hello",
	}, source, peers)
	if err != nil || len(multicast.Deliveries) != 2 || multicast.Deliveries[0].Status != "accepted" || multicast.Deliveries[1].Status != "accepted" {
		t.Fatalf("multicast = %+v, %v", multicast, err)
	}
	broadcast, err := engine.Route(context.Background(), federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "broadcast", MessageID: "broadcast-1",
		Group: "team", Content: "hello",
	}, source, peers)
	if err != nil || len(broadcast.Deliveries) != 2 {
		t.Fatalf("broadcast = %+v, %v", broadcast, err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Catalog.Deliveries) != 4 {
		t.Fatalf("durable deliveries = %+v", snapshot.Catalog.Deliveries)
	}
	for id, delivery := range snapshot.Catalog.Deliveries {
		if delivery.State != "acknowledged" || delivery.Acknowledgment != "destination-accepted" || delivery.RetryCause != "" {
			t.Fatalf("delivery %s = %+v", id, delivery)
		}
	}
	if len(accepted) != 4 {
		t.Fatalf("accepted callbacks = %v", accepted)
	}
}

func TestDeliveryEngineRetryUsesStableIDAndAcknowledgedReplayDoesNotRedispatch(t *testing.T) {
	store := openDeliveryTestState(t)
	var calls []string
	fail := true
	engine, err := NewDeliveryEngine(store, func(
		_ context.Context,
		_, _ federation.Peer,
		deliveryID string,
		_ federation.AgentFrame,
	) error {
		calls = append(calls, deliveryID)
		if fail {
			return errors.New("vendor diagnostic must not persist")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	source, peers := deliveryTestPeers()
	frame := federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "retry-1", Targets: []string{"a"}, Content: "secret body",
	}
	first, err := engine.Route(context.Background(), frame, source, peers)
	if err != nil || len(first.Deliveries) != 1 || first.Deliveries[0].Status != "failed" {
		t.Fatalf("failed delivery = %+v, %v", first, err)
	}
	fail = false
	second, err := engine.Route(context.Background(), frame, source, peers)
	if err != nil || second.Deliveries[0].Status != "accepted" {
		t.Fatalf("retried delivery = %+v, %v", second, err)
	}
	third, err := engine.Route(context.Background(), frame, source, peers)
	if err != nil || third.Deliveries[0].Status != "accepted" {
		t.Fatalf("replayed delivery = %+v, %v", third, err)
	}
	if len(calls) != 2 || calls[0] != calls[1] {
		t.Fatalf("stable presentation calls = %v", calls)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range snapshot.Catalog.Deliveries {
		if delivery.State != "acknowledged" || delivery.RetryCause != "" ||
			delivery.Acknowledgment != "destination-accepted" {
			t.Fatalf("retried record = %+v", delivery)
		}
		if delivery.RetryCause == "vendor diagnostic must not persist" {
			t.Fatal("vendor diagnostic persisted")
		}
	}
}

func TestDeliveryEngineAcknowledgedReplaySurvivesDaemonStateReopen(t *testing.T) {
	root := t.TempDir()
	store, err := OpenState(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	source, peers := deliveryTestPeers()
	frame := federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "durable-replay-1",
		Targets: []string{"a"}, Content: "never persisted",
	}
	calls := 0
	engine, err := NewDeliveryEngine(store, func(context.Context, federation.Peer, federation.Peer, string, federation.AgentFrame) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, routeErr := engine.Route(context.Background(), frame, source, peers); routeErr != nil || result.Deliveries[0].Status != "accepted" {
		t.Fatalf("initial durable delivery = %+v, %v", result, routeErr)
	}
	reopened, err := OpenState(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewDeliveryEngine(reopened, func(context.Context, federation.Peer, federation.Peer, string, federation.AgentFrame) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, routeErr := replay.Route(context.Background(), frame, source, peers); routeErr != nil || result.Deliveries[0].Status != "accepted" {
		t.Fatalf("reopened durable replay = %+v, %v", result, routeErr)
	}
	if calls != 1 {
		t.Fatalf("acknowledged replay was presented %d times, want exactly once", calls)
	}
}

func openDeliveryTestState(t *testing.T) *StateStore {
	t.Helper()
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func deliveryTestPeers() (federation.Peer, []federation.Peer) {
	source := federation.Peer{ID: "source", SessionID: "source", Name: "writer", Groups: []string{"team"}}
	return source, []federation.Peer{
		{ID: "a", SessionID: "a", Name: "reader-a", Groups: []string{"team"}},
		{ID: "b", SessionID: "b", Name: "reader-b", Groups: []string{"team", "other"}},
		{ID: "hidden", SessionID: "hidden", Name: "reader-a", Groups: []string{"secret"}},
	}
}
