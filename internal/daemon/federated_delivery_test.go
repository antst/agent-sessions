package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestFederationComponentDurablyAdmitsGroupAndTerminalDeliveries(t *testing.T) {
	for _, deliveryType := range []string{"group_deliver", "terminal_notice_deliver"} {
		t.Run(deliveryType, func(t *testing.T) {
			fixture := newDeliveryTestFixture(t, nil)
			target := fixture.attach(t, "claude", "target", "target-id", []string{"team"})
			probe := &federatedDeliveryProbeAdapter{state: fixture.state}
			fixture.engine.adapters[target.Product] = probe
			component := &federationComponent{deliveries: fixture.engine}
			delivery := federatedDeliveryRequest(t, deliveryType, "remote-message-1", attachmentAddress(target), "opaque payload")

			if err := component.acceptFederatedDelivery(context.Background(), delivery); err != nil {
				t.Fatalf("accept %s: %v", deliveryType, err)
			}
			record, err := fixture.engine.Read(context.Background(), "remote-message-1")
			if err != nil {
				t.Fatal(err)
			}
			if record.State != DeliveryStateDelivered || record.SourceAttachmentID != "" ||
				record.SourceHostID != "remote-host" || record.ResolvedDestinations[0] != attachmentAddress(target) {
				t.Fatalf("federated delivery record = %#v", record)
			}
			if calls, probeErr := probe.result(); calls != 1 || probeErr != nil {
				t.Fatalf("native delivery calls=%d durable-order error=%v", calls, probeErr)
			}

			if err := component.acceptFederatedDelivery(context.Background(), delivery); err != nil {
				t.Fatalf("identical replay: %v", err)
			}
			if calls, _ := probe.result(); calls != 1 {
				t.Fatalf("identical replay dispatched %d times, want one", calls)
			}

			restarted, err := NewDeliveryEngine(DeliveryEngineOptions{
				State: fixture.state, Attachments: fixture.registry, Now: fixture.clock,
				Adapters: map[string]DeliveryAdapter{"claude": probe},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.AcceptFederated(context.Background(), delivery); err != nil {
				t.Fatalf("identical replay after restart: %v", err)
			}
			if calls, _ := probe.result(); calls != 1 {
				t.Fatalf("restart replay dispatched %d times, want one", calls)
			}

			changed := delivery
			changed.Frame = changedFederatedFrame(t, delivery.Frame, "changed payload")
			if _, err := restarted.AcceptFederated(context.Background(), changed); !errors.Is(err, ErrDeliveryIdempotencyConflict) {
				t.Fatalf("changed-content replay error = %v, want ErrDeliveryIdempotencyConflict", err)
			}
			if calls, _ := probe.result(); calls != 1 {
				t.Fatalf("conflicting replay dispatched %d times, want one", calls)
			}
		})
	}
}

func TestFederatedDeliveryRevalidatesExactCurrentTargetGroupsBeforeAcceptance(t *testing.T) {
	fixture := newDeliveryTestFixture(t, nil)
	target := fixture.attach(t, "claude", "isolated", "isolated-id", []string{"other"})
	delivery := federatedDeliveryRequest(t, "group_deliver", "remote-denied", attachmentAddress(target), "not shared")

	if _, err := fixture.engine.AcceptFederated(context.Background(), delivery); err == nil {
		t.Fatal("federated delivery without a current shared group was accepted")
	}
	if fixture.adapter.callCount() != 0 {
		t.Fatal("unauthorized federated delivery reached the native adapter")
	}
	if _, err := fixture.engine.Read(context.Background(), "remote-denied"); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("pre-acceptance rejection durable record error = %v, want ErrDeliveryNotFound", err)
	}
}

func TestFederatedDeliveryConcurrentIdenticalAdmissionDispatchesOnce(t *testing.T) {
	fixture := newDeliveryTestFixture(t, nil)
	target := fixture.attach(t, "claude", "target", "target-id", []string{"team"})
	delivery := federatedDeliveryRequest(t, "group_deliver", "remote-concurrent", attachmentAddress(target), "one payload")

	const callers = 16
	records := make([]DeliveryRecord, callers)
	errs := make([]error, callers)
	var wait sync.WaitGroup
	for index := range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			records[index], errs[index] = fixture.engine.AcceptFederated(context.Background(), delivery)
		}()
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent caller %d: %v", index, err)
		}
		if !reflect.DeepEqual(records[0], records[index]) {
			t.Fatalf("concurrent caller %d result changed: first=%#v current=%#v", index, records[0], records[index])
		}
	}
	if calls := fixture.adapter.callCount(); calls != 1 {
		t.Fatalf("concurrent identical delivery dispatched %d times, want one", calls)
	}
}

func federatedDeliveryRequest(t *testing.T, deliveryType, messageID, targetID, content string) federation.RoutedDelivery {
	t.Helper()
	source := federation.Peer{
		ID: "remote-host/source", HostID: "remote-host", HostName: "remote", SessionID: "source",
		GlobalID: federation.GlobalSessionID("remote-host", "source"), Name: "source",
		DisplayName: "source--remote", Entrypoint: "codex", PermissionMode: "default",
		PeerProtocol: federation.GroupProtocolVersion, InstanceID: "remote-source-instance",
		Groups: []string{"session:remote-host/source", "team"},
	}
	frame, err := json.Marshal(federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "delivery", MessageID: messageID,
		SourceSessionID: source.SessionID, Source: &source, Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return federation.RoutedDelivery{
		Type: deliveryType, RequestID: "route-" + messageID,
		SourceID: source.ID, TargetID: targetID, Frame: frame,
	}
}

func changedFederatedFrame(t *testing.T, body json.RawMessage, content string) json.RawMessage {
	t.Helper()
	var frame federation.AgentFrame
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	frame.Content = content
	changed, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

type federatedDeliveryProbeAdapter struct {
	mu    sync.Mutex
	state *StateStore
	calls int
	err   error
}

func (adapter *federatedDeliveryProbeAdapter) Deliver(
	ctx context.Context,
	_ AttachmentRecord,
	frame federation.AgentFrame,
) error {
	catalog, _, err := adapter.state.readDeliveryCatalog(ctx)
	if err == nil {
		found := false
		for _, record := range catalog.Records {
			if record.MessageID == frame.MessageID && record.State == DeliveryStateAccepted &&
				record.DestinationResults[record.ResolvedDestinations[0]] == DeliveryDestinationPending {
				found = true
				break
			}
		}
		if !found {
			err = errors.New("native delivery ran before durable accepted state")
		}
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.calls++
	adapter.err = errors.Join(adapter.err, err)
	return err
}

func (adapter *federatedDeliveryProbeAdapter) result() (int, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.calls, adapter.err
}
