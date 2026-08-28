package federation

import (
	"errors"
	"reflect"
	"testing"
)

func TestRemoteLaneRouteTableAuthenticatesExactSourceTargetAndTerminalOwnership(t *testing.T) {
	source := federationRouteTestPeer("host-a", "source", "source", "codex")
	target := Host{ID: "host-b", Name: "host-b", Capabilities: []string{"qwen-lane"}}
	envelope := RemoteLaneEnvelope{
		RequestID: "request-1", SourceID: source.ID, TargetHostID: target.ID,
		Parent: source, Product: "qwen", LaneSessionID: "lane-1", TurnID: "turn-1",
		Name: "worker", Cwd: "/tmp/work", PermissionMode: "default",
	}
	var routes RemoteLaneRouteTable
	decision, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Route.SourceHostID != "host-a" || decision.Route.TargetHostID != "host-b" ||
		decision.Route.SourceID != source.ID || decision.Route.LaneSessionID != envelope.LaneSessionID ||
		!reflect.DeepEqual(decision.Envelope, envelope) || routes.Counts() != 1 {
		t.Fatalf("accepted lane route = %+v, count=%d", decision, routes.Counts())
	}
	if replay, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", envelope); err != nil || replay.Route != decision.Route {
		t.Fatalf("exact remote lane replay did not converge: %+v, %v", replay, err)
	}
	changed := envelope
	changed.TurnID = "turn-forged"
	if _, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", changed); !errors.Is(err, ErrRemoteLaneIdempotencyConflict) {
		t.Fatalf("changed remote lane replay error = %v", err)
	}
	if _, ok := routes.Cancel(envelope.RequestID, "host-forged"); ok {
		t.Fatal("forged source cancelled an accepted remote lane")
	}
	if _, ok := routes.Cancel(envelope.RequestID, "host-a"); !ok {
		t.Fatal("exact source could not cancel its remote lane")
	}
	if _, ok := routes.Outcome(envelope.RequestID, "host-forged", true); ok {
		t.Fatal("forged destination completed an accepted remote lane")
	}
	if _, ok := routes.Outcome(envelope.RequestID, "host-b", false); !ok || routes.Counts() != 1 {
		t.Fatal("authenticated progress did not retain the remote lane route")
	}
	if _, ok := routes.Outcome(envelope.RequestID, "host-b", true); !ok || routes.Counts() != 0 {
		t.Fatal("authenticated terminal outcome did not consume the remote lane route")
	}
}

func TestRemoteLaneRouteTableRejectsCapabilityAndParentForgeryBeforeAcceptance(t *testing.T) {
	source := federationRouteTestPeer("host-a", "source", "source", "codex")
	target := Host{ID: "host-b", Name: "host-b", Capabilities: []string{"qwen-lane"}}
	envelope := RemoteLaneEnvelope{
		RequestID: "request-1", SourceID: source.ID, TargetHostID: target.ID,
		Parent: source, Product: "qwen", LaneSessionID: "lane-1", TurnID: "turn-1",
		Name: "worker", Cwd: "/tmp/work", PermissionMode: "default",
	}
	var routes RemoteLaneRouteTable
	forged := envelope
	forged.Parent.InstanceID = "forged-instance"
	if _, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", forged); !errors.Is(err, ErrRemoteLaneParentMismatch) {
		t.Fatalf("forged parent error = %v", err)
	}
	unsupported := target
	unsupported.Capabilities = []string{"codex-lane"}
	if _, err := routes.Begin(RegistrySnapshot{Hosts: []Host{unsupported}, Peers: []Peer{source}}, "host-a", envelope); err == nil {
		t.Fatal("target without requested product capability accepted a remote lane")
	}
	if routes.Counts() != 0 {
		t.Fatal("rejected remote lane requests mutated route ownership")
	}
	forged = envelope
	forged.Parent.Groups = []string{"forged-group"}
	forged.Parent.Entrypoint = "grok"
	forged.Parent.PermissionMode = "bypassPermissions"
	decision, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", forged)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decision.Envelope.Parent, source) {
		t.Fatalf("hub forwarded caller-supplied parent authority: got %+v want %+v", decision.Envelope.Parent, source)
	}
}

func TestRemoteLaneRouteTableConsumesExactTerminalNoticeAndClearsDisconnects(t *testing.T) {
	source := federationRouteTestPeer("host-a", "source", "source", "codex")
	target := Host{ID: "host-b", Name: "host-b", Capabilities: []string{"qwen-lane"}}
	envelope := RemoteLaneEnvelope{
		RequestID: "request-1", SourceID: source.ID, TargetHostID: target.ID,
		Parent: source, Product: "qwen", LaneSessionID: "lane-1", TurnID: "turn-1",
		Name: "worker", Cwd: "/tmp/work", PermissionMode: "default",
	}
	var routes RemoteLaneRouteTable
	if _, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := routes.AuthorizeNotice("host-forged", "lane-1", "host-a", "source"); ok {
		t.Fatal("forged terminal notice consumed an accepted lane route")
	}
	if route, ok := routes.AuthorizeNotice("host-b", "lane-1", "host-a", "source"); !ok || routes.Counts() != 1 {
		t.Fatal("exact terminal notice was not authorized without premature consumption")
	} else if !routes.CompleteNotice(route.RequestID, "host-b", "host-a") || routes.Counts() != 0 {
		t.Fatal("acknowledged terminal notice did not consume the accepted lane route")
	} else if _, ok := routes.AuthorizeNotice("host-b", "lane-1", "host-a", "source"); !ok {
		t.Fatal("acknowledged route was not retained until acknowledgement delivery")
	} else if !routes.FinalizeNotice(route.RequestID, "host-b", "host-a") {
		t.Fatal("delivered acknowledgement did not finalize its exact lane route")
	} else if _, ok := routes.AuthorizeNotice("host-b", "lane-1", "host-a", "source"); ok {
		t.Fatal("finalized terminal route was retained")
	}

	envelope.RequestID = "request-2"
	if _, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", envelope); err != nil {
		t.Fatal(err)
	}
	failed := routes.DropHost("host-b")
	if len(failed) != 0 || routes.Counts() != 1 {
		t.Fatalf("destination restart discarded accepted work: failures %+v count %d", failed, routes.Counts())
	}
	if _, ok := routes.Outcome(envelope.RequestID, "host-b", true); !ok {
		t.Fatal("reconnected destination could not finish its accepted route")
	}
	envelope.RequestID = "request-3"
	if _, err := routes.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", envelope); err != nil {
		t.Fatal(err)
	}
	if failed := routes.DropHost("host-a"); len(failed) != 0 || routes.Counts() != 1 {
		t.Fatalf("source restart discarded accepted work: %+v count %d", failed, routes.Counts())
	}
}

func TestRemoteLaneArchiveRouteAuthenticatesSourceTargetAndOneResult(t *testing.T) {
	source := federationRouteTestPeer("host-a", "parent", "parent", "codex")
	target := Host{ID: "host-b", Name: "host-b", Capabilities: []string{"qwen-lane"}}
	request := RemoteLaneArchive{
		RequestID: "archive-1", RemoteRequestID: "request-1", SourceID: source.ID,
		TargetHostID: target.ID, Product: "qwen", LaneSessionID: "lane-1",
	}
	table := &RemoteLaneArchiveRouteTable{}
	route, err := table.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-a", request)
	if err != nil || route.SourceHostID != "host-a" || route.TargetHostID != "host-b" {
		t.Fatalf("archive route = %#v, %v", route, err)
	}
	if _, err := table.Begin(RegistrySnapshot{Hosts: []Host{target}, Peers: []Peer{source}}, "host-forged", request); err == nil {
		t.Fatal("forged archive source reused the route")
	}
	if _, ok := table.Complete(request.RequestID, "host-forged"); ok {
		t.Fatal("forged archive destination completed the route")
	}
	if got, ok := table.Complete(request.RequestID, "host-b"); !ok || got != route {
		t.Fatalf("archive completion = %#v, %t", got, ok)
	}
	if _, ok := table.Complete(request.RequestID, "host-b"); ok {
		t.Fatal("duplicate archive completion was accepted")
	}
}
