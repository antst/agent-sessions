package federation

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCapabilityPublicationAndHubRegistryAreProtocolAndGenerationBound(t *testing.T) {
	capabilities, err := CapabilitiesForReadyProducts([]string{"qwen", "codex", "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex-lane", "qwen-lane"}; !reflect.DeepEqual(capabilities, want) {
		t.Fatalf("capabilities = %q, want %q", capabilities, want)
	}
	registry := NewHubRegistry()
	advertisement := HostAdvertisement{
		HostID: "host-a", HostName: "workstation", ProtocolVersion: ProtocolVersion,
		RuntimeVersion: "unrelated-build-a", RuntimeIdentity: "sha256:" + strings.Repeat("a", 64),
		Generation: 4, Products: []string{"qwen", "codex", "qwen"},
		Capabilities: []string{"qwen-lane", "unknown-future-lane", "codex-lane", "qwen-lane"},
	}
	owner, err := registry.RegisterHost(advertisement)
	if err != nil {
		t.Fatal(err)
	}
	peer := federationRouteTestPeer("host-a", "session-a", "alpha", "codex")
	local, err := NewLocalCatalog("host-a", "workstation")
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Replace([]Peer{peer}); err != nil {
		t.Fatal(err)
	}
	peer.Groups[0] = "mutated-after-publication"
	if got := local.Snapshot(); len(got) != 1 || got[0].Groups[0] != "project" {
		t.Fatalf("local catalog did not own an independent snapshot: %+v", got)
	}
	peer = federationRouteTestPeer("host-a", "session-a", "alpha", "codex")
	if err := registry.ReplaceHostSnapshot("host-a", owner, []Peer{peer}); err != nil {
		t.Fatal(err)
	}
	replacement := advertisement
	replacement.RuntimeVersion = "unrelated-build-b"
	replacement.RuntimeIdentity = "sha256:" + strings.Repeat("b", 64)
	replacement.Generation++
	newOwner, err := registry.RegisterHost(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if newOwner == owner {
		t.Fatal("replacement host transport reused its ownership generation")
	}
	if registry.UnregisterHost("host-a", owner) {
		t.Fatal("stale transport unregistered its successor")
	}
	if got := registry.Snapshot(); len(got.Hosts) != 1 || len(got.Peers) != 0 ||
		!reflect.DeepEqual(got.Hosts[0].Capabilities, capabilities) {
		t.Fatalf("replacement registry snapshot = %+v", got)
	}

	mismatch := advertisement
	mismatch.HostID = "host-mismatch"
	mismatch.ProtocolVersion++
	if _, err := registry.RegisterHost(mismatch); err == nil {
		t.Fatal("protocol mismatch registered with the hub")
	}
	if got := registry.Snapshot(); len(got.Hosts) != 1 {
		t.Fatalf("protocol mismatch mutated registry: %+v", got)
	}
}

func TestRouterUsesGlobalGroupsHostSuffixesAndIdempotentMessageAdmission(t *testing.T) {
	peers := []Peer{
		federationRouteTestPeer("host-a", "source", "source", "codex"),
		federationRouteTestPeer("host-b", "target-b", "worker", "claude"),
		federationRouteTestPeer("host-c", "target-c", "worker", "qwen"),
	}
	var (
		mu         sync.Mutex
		deliveries []string
	)
	router, err := NewRouter(RouterOptions{
		Snapshot: func(context.Context) ([]Peer, error) { return append([]Peer(nil), peers...), nil },
		Deliver: func(_ context.Context, _ Peer, target Peer, _ AgentFrame) error {
			mu.Lock()
			defer mu.Unlock()
			deliveries = append(deliveries, target.ID)
			return nil
		},
		Now: func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "ambiguous",
		Targets: []string{"worker"}, Content: "must not route",
	}
	if _, err := router.Route(context.Background(), "host-a/source", ambiguous); err == nil {
		t.Fatal("ambiguous global name routed without a host suffix")
	}
	frame := AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "message-1",
		Targets: []string{"worker--host-b"}, Content: "deliver once",
	}
	first, err := router.Route(context.Background(), "host-a/source", frame)
	if err != nil {
		t.Fatal(err)
	}
	second, err := router.Route(context.Background(), "host-a/source", frame)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent replay changed result: first=%+v second=%+v", first, second)
	}
	mu.Lock()
	gotDeliveries := append([]string(nil), deliveries...)
	mu.Unlock()
	if want := []string{"host-b/target-b"}; !reflect.DeepEqual(gotDeliveries, want) {
		t.Fatalf("native deliveries = %q, want one exact delivery %q", gotDeliveries, want)
	}
	changed := frame
	changed.Content = "conflict"
	if _, err := router.Route(context.Background(), "host-a/source", changed); !errors.Is(err, ErrMessageIdempotencyConflict) {
		t.Fatalf("changed replay error = %v, want ErrMessageIdempotencyConflict", err)
	}
}

func TestHubDeliveryAdmissionAndOutcomeOwnershipFailClosed(t *testing.T) {
	source := federationRouteTestPeer("host-a", "source", "source", "codex")
	target := federationRouteTestPeer("host-b", "target", "target", "claude")
	delivered := AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: "message-1",
		SourceSessionID: source.SessionID, Source: &source, Content: "opaque",
	}
	body, err := json.Marshal(delivered)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := ResolveHubDelivery(RegistrySnapshot{Peers: []Peer{source, target}}, HubDeliveryRequest{
		Type: "group_deliver", RequestID: "request-1", SourceHostID: "host-a",
		SourceID: source.ID, TargetID: target.ID, Frame: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.TargetHostID != "host-b" || decision.Target.ID != target.ID {
		t.Fatalf("hub route decision = %+v", decision)
	}
	var routes DeliveryRouteTable
	route := DeliveryRoute{
		RequestID: decision.RequestID, SourceHostID: decision.SourceHostID, SourceID: source.ID,
		TargetHostID: decision.TargetHostID, TargetID: target.ID,
	}
	if err := routes.Begin(route); err != nil {
		t.Fatal(err)
	}
	if _, ok := routes.Resolve(route.RequestID, "host-forged", route.SourceID, route.TargetID); ok {
		t.Fatal("forged outcome consumed an active delivery route")
	}
	if got := routes.Counts().Pending; got != 1 {
		t.Fatalf("pending routes after forged outcome = %d, want 1", got)
	}
	if resolved, ok := routes.Resolve(route.RequestID, route.TargetHostID, route.SourceID, route.TargetID); !ok || !reflect.DeepEqual(resolved, route) {
		t.Fatalf("exact outcome route = %+v, ok=%t", resolved, ok)
	}
	if got := routes.Counts().Pending; got != 0 {
		t.Fatalf("pending routes after exact outcome = %d", got)
	}
	disconnected := route
	disconnected.RequestID = "request-disconnected"
	if err := routes.Begin(disconnected); err != nil {
		t.Fatal(err)
	}
	if err := routes.Begin(disconnected); err == nil {
		t.Fatal("duplicate transient request id was admitted")
	}
	if failed := routes.DropHost("host-b"); !reflect.DeepEqual(failed, []DeliveryRoute{disconnected}) {
		t.Fatalf("destination disconnect failures = %+v", failed)
	}
	if got := routes.Counts().Pending; got != 0 {
		t.Fatalf("pending routes after disconnect = %d", got)
	}
}

func federationRouteTestPeer(hostID, sessionID, name, product string) Peer {
	return Peer{
		ID: hostID + "/" + sessionID, HostID: hostID, HostName: hostID, SessionID: sessionID,
		GlobalID: GlobalSessionID(hostID, sessionID), Name: name,
		DisplayName: QualifiedPeerName(name, hostID), Entrypoint: product,
		PeerProtocol: GroupProtocolVersion, InstanceID: "instance-" + sessionID,
		Groups: []string{"project", "session:" + hostID + "/" + sessionID},
	}
}
