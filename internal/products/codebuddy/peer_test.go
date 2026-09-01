package codebuddy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestPeerAdoptReattestsRegistrySocketProcessAndExactLiveSession(t *testing.T) {
	var replies, renames int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get(CSRFHeader) != CSRFValue || request.Header.Get("Authorization") != "" {
			http.Error(response, "forbidden", http.StatusForbidden)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/sessions/live":
			writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"sessionId": "native-1", "writerOccupied": false}})
		case "POST /api/v1/sessions/native-1/reply":
			replies++
			writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"delivered": true}})
		case "POST /api/v1/sessions/native-1/rename":
			renames++
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	attachment, wrapper, worker := attachmentFixture(server.URL)
	registry := &fakeRegistry{claim: WorkerClaim{
		SessionID: "native-1", PID: worker.PID, Kind: "interactive", Cwd: "/work", Endpoint: server.URL,
		Registry: RegistryIdentity{Path: "/home/user/.codebuddy/sessions/101.json", Device: 1, Inode: 2, Bytes: 100},
	}}
	processes := &fakeProcesses{
		identities: map[int]procinfo.Identity{wrapper.PID: wrapper, worker.PID: worker}, executable: "/usr/bin/node",
		argv: []string{"node", "/opt/codebuddy.js"}, descends: map[[2]int]bool{{worker.PID, wrapper.PID}: true},
	}
	peer, message, err := NewPeerDriver(codebuddyTestConfig(t, registry, processes), productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := peer.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	attachment.State = "selecting"
	evidence, err := adapter.Adopt(context.Background(), attachment, daemon.NativeEvidence{Process: wrapper, SocketPath: "attacker-claim"})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Process != worker || evidence.Executable != "/usr/bin/node" || evidence.RegistryPath != registry.claim.Registry.Path || evidence.SocketPath != "" {
		t.Fatalf("adopt evidence = %#v", evidence)
	}
	attachment.State, attachment.Evidence = "attached", evidence
	restartedPeer, _, err := NewPeerDriver(codebuddyTestConfig(t, registry, processes), productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	restartedAdapter, _ := restartedPeer.AttachmentAdapter(productruntime.HostDeps{})
	refreshed, err := restartedAdapter.Refresh(context.Background(), attachment)
	if err != nil || refreshed.Process != worker {
		t.Fatalf("daemon-restart refresh = %#v, %v", refreshed, err)
	}
	if _, err := message.Deliver(context.Background(), attachment, productruntime.DeliveryRequest{
		DeliveryID: "delivery-1", Mode: productruntime.DeliveryIdleWake, Body: []byte("wake now"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := message.Deliver(context.Background(), attachment, productruntime.DeliveryRequest{
		DeliveryID: "delivery-2", Mode: productruntime.DeliveryBusySteer, Body: []byte("steer now"),
	}); err != nil {
		t.Fatal(err)
	}
	name, err := peer.Rename(context.Background(), attachment, "new name")
	if err != nil || name.Applied != "new name" || !name.NativeConfirmed {
		t.Fatalf("rename = %#v, %v", name, err)
	}
	if replies != 2 || renames != 1 || registry.finds != 5 {
		t.Fatalf("calls replies=%d renames=%d registry=%d", replies, renames, registry.finds)
	}
}

func TestPeerRejectsStalePortReuseAndCrossTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"sessionId": "other-session", "writerOccupied": false}})
	}))
	defer server.Close()
	attachment, wrapper, worker := attachmentFixture(server.URL)
	attachment.State = "selecting"
	registry := &fakeRegistry{claim: WorkerClaim{SessionID: "native-1", PID: worker.PID, Kind: "interactive", Cwd: "/work", Endpoint: server.URL}}
	processes := &fakeProcesses{
		identities: map[int]procinfo.Identity{wrapper.PID: wrapper, worker.PID: worker}, executable: "/usr/bin/node",
		argv: []string{"node", "codebuddy.js"}, descends: map[[2]int]bool{{worker.PID, wrapper.PID}: true},
	}
	config := codebuddyTestConfig(t, registry, processes)
	peer, _, err := NewPeerDriver(config, productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	adapter, _ := peer.AttachmentAdapter(productruntime.HostDeps{})
	if _, err := adapter.Adopt(context.Background(), attachment, daemon.NativeEvidence{Process: wrapper}); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("cross-target error = %v", err)
	}

	config.SocketOwner = SocketOwnerVerifierFunc(func(context.Context, string, int) (SocketObservation, error) {
		return SocketObservation{}, ErrSocketOwner
	})
	peer, _, _ = NewPeerDriver(config, productruntime.HostDeps{})
	adapter, _ = peer.AttachmentAdapter(productruntime.HostDeps{})
	if _, err := adapter.Adopt(context.Background(), attachment, daemon.NativeEvidence{Process: wrapper}); !errors.Is(err, ErrSocketOwner) {
		t.Fatalf("port-reuse error = %v", err)
	}

	config = codebuddyTestConfig(t, registry, processes)
	correct := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"sessionId": "native-1", "writerOccupied": false}})
	}))
	defer correct.Close()
	registry.claim.Endpoint = correct.URL
	registry.verifyErr = productruntime.ErrStale
	peer, _, _ = NewPeerDriver(config, productruntime.HostDeps{})
	adapter, _ = peer.AttachmentAdapter(productruntime.HostDeps{})
	if _, err := adapter.Adopt(context.Background(), attachment, daemon.NativeEvidence{Process: wrapper}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("changed registry error = %v", err)
	}
}

func TestPeerLaunchInjectsOnlyConnectorIdentityAndNoSecret(t *testing.T) {
	registry := &fakeRegistry{}
	processes := &fakeProcesses{}
	peer, _, err := NewPeerDriver(codebuddyTestConfig(t, registry, processes), productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	command, err := peer.BuildLaunch(context.Background(), productruntime.PeerLaunchRequest{
		ProductID: ProductID, AttachmentID: "peer-1", Cwd: "/work", Args: []string{"--name", "alpha"},
		Env: []productruntime.EnvVar{{Name: "PATH", Value: "/bin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(command.SensitiveEnv) != 0 || strings.Contains(strings.Join(command.Args, " "), "password") ||
		argumentValue(command.Args, "--session-id") != "peer-1" || argumentValue(command.Args, "--mcp-config") == "" {
		t.Fatalf("launch command = %#v", command)
	}
	for _, variable := range command.Env {
		if variable.Name == GatewayPasswordEnv {
			t.Fatal("peer launch acquired a password")
		}
	}
	for _, arguments := range [][]string{
		{"--", "prompt text"},
		{"--", "--session-id", "attacker"},
		{"--", "--mcp-config", "/tmp/attacker.json"},
	} {
		if _, err := peer.BuildLaunch(context.Background(), productruntime.PeerLaunchRequest{
			ProductID: ProductID, AttachmentID: "peer-1", Cwd: "/work", Args: arguments,
		}); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
			t.Fatalf("option terminator %q was accepted: %v", arguments, err)
		}
	}
}

func TestEntrypointMatcherDoesNotTrustPromptArguments(t *testing.T) {
	if !DefaultEntrypointMatcher("/usr/bin/node", []string{"node", "/opt/codebuddy"}) {
		t.Fatal("exact Node script entrypoint was rejected")
	}
	if DefaultEntrypointMatcher("/usr/bin/node", []string{"node", "/opt/attacker.js", "codebuddy"}) {
		t.Fatal("product-looking prompt argument was accepted as an entrypoint")
	}
}
