package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestDeliveryDiscoveryUsesOnlyExistingGlobalGroups(t *testing.T) {
	fixture := newDeliveryTestFixture(t, nil)
	source := fixture.attach(t, "codex", "source", "source-id", []string{"team", "private:source"})
	visible := fixture.attach(t, "claude", "visible", "visible-id", []string{"team"})
	fixture.attach(t, "grok", "isolated", "isolated-id", []string{"other"})

	peers, err := fixture.engine.Discover(context.Background(), source.AttachmentID)
	if err != nil {
		t.Fatalf("discover peers: %v", err)
	}
	if len(peers) != 1 || peers[0].SessionID != visible.SessionID || peers[0].DisplayName != "visible--host-test" {
		t.Fatalf("visible peers = %#v", peers)
	}
}

func TestDeliveryDirectMulticastAndBroadcastRequireCompleteAdmission(t *testing.T) {
	fixture := newDeliveryTestFixture(t, nil)
	source := fixture.attach(t, "codex", "source", "source-id", []string{"team"})
	first := fixture.attach(t, "claude", "first", "first-id", []string{"team"})
	second := fixture.attach(t, "qwen", "second", "second-id", []string{"team", "other"})
	isolated := fixture.attach(t, "grok", "isolated", "isolated-id", []string{"other"})

	direct, err := fixture.engine.Accept(context.Background(), DeliveryRequest{
		MessageID: "direct-1", SourceAttachmentID: source.AttachmentID, Operation: DeliveryOperationSend,
		Targets: []string{"host-test/" + first.SessionID}, Content: "hello direct",
	})
	if err != nil {
		t.Fatalf("accept direct delivery: %v", err)
	}
	if direct.State != DeliveryStateDelivered || direct.DestinationResults["host-test/"+first.SessionID] != DeliveryDestinationDelivered {
		t.Fatalf("direct delivery = %#v", direct)
	}

	beforeCalls := fixture.adapter.callCount()
	if _, err := fixture.engine.Accept(context.Background(), DeliveryRequest{
		MessageID: "multicast-reject", SourceAttachmentID: source.AttachmentID, Operation: DeliveryOperationMulticast,
		Targets: []string{"host-test/" + second.SessionID, "host-test/" + isolated.SessionID}, Content: "must be atomic",
	}); !errors.Is(err, ErrDeliveryUnauthorized) {
		t.Fatalf("partially unauthorized multicast error = %v, want ErrDeliveryUnauthorized", err)
	}
	if fixture.adapter.callCount() != beforeCalls {
		t.Fatal("partially unauthorized multicast delivered to an admitted prefix")
	}
	if _, err := fixture.engine.Read(context.Background(), "multicast-reject"); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("rejected multicast durable record error = %v, want ErrDeliveryNotFound", err)
	}

	broadcast, err := fixture.engine.Accept(context.Background(), DeliveryRequest{
		MessageID: "broadcast-1", SourceAttachmentID: source.AttachmentID, Operation: DeliveryOperationBroadcast,
		Group: "team", Content: "hello group",
	})
	if err != nil {
		t.Fatalf("accept group broadcast: %v", err)
	}
	wantDestinations := []string{"host-test/first-id", "host-test/second-id"}
	if !reflect.DeepEqual(broadcast.ResolvedDestinations, wantDestinations) {
		t.Fatalf("broadcast destinations = %q, want %q", broadcast.ResolvedDestinations, wantDestinations)
	}
	if _, err := fixture.engine.Accept(context.Background(), DeliveryRequest{
		MessageID: "broadcast-reject", SourceAttachmentID: source.AttachmentID, Operation: DeliveryOperationBroadcast,
		Group: "other", Content: "unauthorized group",
	}); !errors.Is(err, ErrDeliveryUnauthorized) {
		t.Fatalf("unauthorized broadcast error = %v, want ErrDeliveryUnauthorized", err)
	}
}

func TestDeliveryAcceptanceIsDurableRetryableAndAtMostOncePerDestination(t *testing.T) {
	fixture := newDeliveryTestFixture(t, nil)
	source := fixture.attach(t, "codex", "source", "source-id", []string{"team"})
	target := fixture.attach(t, "claude", "target", "target-id", []string{"team"})
	request := DeliveryRequest{
		MessageID: "retry-1", SourceAttachmentID: source.AttachmentID, Operation: DeliveryOperationSend,
		Targets: []string{"host-test/" + target.SessionID}, Content: "durable payload", Summary: "retry",
	}

	first, err := fixture.engine.Accept(context.Background(), request)
	if err != nil {
		t.Fatalf("first acceptance: %v", err)
	}
	if first.AcceptedRevision == 0 || first.AcceptedAt == 0 {
		t.Fatalf("delivery was reported before durable acceptance: %#v", first)
	}
	second, err := fixture.engine.Accept(context.Background(), request)
	if err != nil {
		t.Fatalf("retry accepted request: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent retry changed result: first=%#v second=%#v", first, second)
	}
	if got := fixture.adapter.deliveriesTo(target.SessionID); got != 1 {
		t.Fatalf("destination delivery count = %d, want exactly one", got)
	}

	restarted, err := NewDeliveryEngine(DeliveryEngineOptions{
		State: fixture.state, Attachments: fixture.registry,
		Adapters: map[string]DeliveryAdapter{"claude": fixture.adapter}, Now: fixture.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	third, err := restarted.Accept(context.Background(), request)
	if err != nil {
		t.Fatalf("retry after daemon reconstruction: %v", err)
	}
	if !reflect.DeepEqual(first, third) || fixture.adapter.deliveriesTo(target.SessionID) != 1 {
		t.Fatalf("restart retry duplicated delivery: first=%#v third=%#v calls=%d", first, third, fixture.adapter.callCount())
	}

	changed := request
	changed.Content = "different payload under same id"
	if _, err := restarted.Accept(context.Background(), changed); !errors.Is(err, ErrDeliveryIdempotencyConflict) {
		t.Fatalf("changed retry error = %v, want ErrDeliveryIdempotencyConflict", err)
	}
}

func TestDeliveryDiagnosticsNeverContainMessageContent(t *testing.T) {
	const canary = "MESSAGE_CONTENT_MUST_NOT_REACH_LOGS_91bce6"
	observations := make([]DeliveryObservation, 0, 4)
	fixture := newDeliveryTestFixture(t, func(observation DeliveryObservation) {
		observations = append(observations, observation)
	})
	source := fixture.attach(t, "codex", "source", "source-id", []string{"team"})
	target := fixture.attach(t, "qwen", "target", "target-id", []string{"team"})
	if _, err := fixture.engine.Accept(context.Background(), DeliveryRequest{
		MessageID: "canary-1", SourceAttachmentID: source.AttachmentID, Operation: DeliveryOperationSend,
		Targets: []string{"host-test/" + target.SessionID}, Content: canary, Summary: canary,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(observations)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), canary) {
		t.Fatalf("delivery observation leaked content: %s", body)
	}
	if len(observations) == 0 || observations[len(observations)-1].MessageID != "canary-1" {
		t.Fatalf("metadata-only delivery observation = %#v", observations)
	}
}

func TestDeliveryResourceBudgetsFailBeforeAcceptanceOrNativeWork(t *testing.T) {
	for _, resource := range []string{"disk", "memory", "file_descriptors", "processes"} {
		t.Run(resource, func(t *testing.T) {
			budgetErr := &DeliveryResourceError{Resource: resource}
			fixture := newDeliveryTestFixture(t, nil)
			fixture.engine.preflight = func(context.Context, DeliveryRequest) error { return budgetErr }
			source := fixture.attach(t, "codex", "source", "source-id", []string{"team"})
			target := fixture.attach(t, "grok", "target", "target-id", []string{"team"})
			beforeCalls := fixture.adapter.callCount()
			if _, err := fixture.engine.Accept(context.Background(), DeliveryRequest{
				MessageID: "budget-" + resource, SourceAttachmentID: source.AttachmentID,
				Operation: DeliveryOperationSend, Targets: []string{"host-test/" + target.SessionID}, Content: "bounded",
			}); !errors.Is(err, budgetErr) {
				t.Fatalf("%s budget error = %v, want %v", resource, err, budgetErr)
			}
			if fixture.adapter.callCount() != beforeCalls {
				t.Fatalf("%s budget rejection started native delivery", resource)
			}
			if _, err := fixture.engine.Read(context.Background(), "budget-"+resource); !errors.Is(err, ErrDeliveryNotFound) {
				t.Fatalf("%s budget rejection durable record error = %v", resource, err)
			}
		})
	}
}

type deliveryTestFixture struct {
	state    *StateStore
	registry *AttachmentRegistry
	engine   *DeliveryEngine
	adapter  *deliveryTestAdapter
	clock    func() time.Time
}

func newDeliveryTestFixture(t *testing.T, observe func(DeliveryObservation)) *deliveryTestFixture {
	t.Helper()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	clock := attachmentTestClock()
	actor := &attachmentTestAdapter{}
	registry, err := NewAttachmentRegistry(AttachmentRegistryOptions{
		State: state, Generation: 9, HostID: "host-test", Now: clock,
		Capability: func() (string, error) { return "delivery-capability", nil },
		Adapters:   map[string]AttachmentAdapter{"codex": actor, "claude": actor, "grok": actor, "qwen": actor},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &deliveryTestAdapter{counts: make(map[string]int)}
	engine, err := NewDeliveryEngine(DeliveryEngineOptions{
		State: state, Attachments: registry, Now: clock, Observe: observe,
		Adapters: map[string]DeliveryAdapter{"codex": adapter, "claude": adapter, "grok": adapter, "qwen": adapter},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &deliveryTestFixture{state: state, registry: registry, engine: engine, adapter: adapter, clock: clock}
}

func (fixture *deliveryTestFixture) attach(t *testing.T, product, name, sessionID string, groups []string) AttachmentRecord {
	t.Helper()
	prepared, capability, err := fixture.registry.Prepare(context.Background(), AttachmentPrepareRequest{
		Product: product, Kind: "interactive", Cwd: "/work", Name: name, Groups: groups,
		ExpectedNativeActor: map[string]any{"pid": len(sessionID) + 1000, "proc_start": "stable-" + sessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := fixture.registry.Adopt(context.Background(), AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: sessionID,
		NativeActor: map[string]any{"pid": len(sessionID) + 1000, "proc_start": "stable-" + sessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return attached
}

type deliveryTestAdapter struct {
	counts map[string]int
	frames []federation.AgentFrame
}

func (adapter *deliveryTestAdapter) Deliver(_ context.Context, destination AttachmentRecord, frame federation.AgentFrame) error {
	adapter.counts[destination.SessionID]++
	adapter.frames = append(adapter.frames, frame)
	return nil
}

func (adapter *deliveryTestAdapter) callCount() int {
	count := 0
	for _, value := range adapter.counts {
		count += value
	}
	return count
}

func (adapter *deliveryTestAdapter) deliveriesTo(sessionID string) int {
	return adapter.counts[sessionID]
}
