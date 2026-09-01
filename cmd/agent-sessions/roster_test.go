package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/clihelp"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
)

func TestOperatorRosterProjectsCurrentLocalAndFederatedMetadataWithoutSensitiveContent(t *testing.T) {
	const secret = "SECRET-PROMPT-RESULT-CREDENTIAL"
	attachments := []daemonpkg.ManagedAttachment{{
		ID: "local-peer", Product: "codex", NativeSessionID: "native-peer",
		Cwd: "/work", Groups: []string{"project"}, PermissionMode: "bypassPermissions",
		State: "attached", DaemonGeneration: 7,
		CapabilityHash: secret, Evidence: daemonpkg.NativeEvidence{Executable: secret},
	}}
	remoteHosts := []federationpkg.Host{{
		ID: "host-b", Name: "workstation-b", Generation: 4, Build: "0.3.0",
		Capabilities: []string{federationpkg.CapabilityCodexLane},
	}}
	remotePeers := []federationpkg.Peer{
		{
			GlobalID: "global-peer", SessionID: "remote-peer", Name: "analyst", DisplayName: "analyst--workstation-b",
			HostID: "host-b", HostName: "workstation-b", Product: "claude", Status: "idle",
			Cwd: "/remote", Groups: []string{"project", "session:host-b/remote-peer"},
		},
		{
			GlobalID: "global-lane", SessionID: "remote-lane", Name: "builder", DisplayName: "builder--workstation-b",
			HostID: "host-b", HostName: "workstation-b", Product: "grok", Status: "busy",
			Cwd: "/remote", Groups: []string{"project", "session:host-b/remote-lane"}, ParentSessionID: "remote-peer",
		},
	}
	report := buildOperatorRoster(attachments, 7, "host-a", "0.3.0", "workstation-a", true, true, remoteHosts, remotePeers)
	if report.Schema != operatorRosterSchema || report.Host.ID != "host-a" || !report.Host.FederationConnected {
		t.Fatalf("host roster = %+v", report.Host)
	}
	if report.Summary != (operatorRosterSummary{LocalPeers: 1, RemotePeers: 1, RemoteLanes: 1, FederatedHosts: 1}) {
		t.Fatalf("roster summary = %+v", report.Summary)
	}
	if len(report.Local) != 1 || len(report.Remote) != 2 || len(report.FederatedHosts) != 1 {
		t.Fatalf("roster sizes local=%d remote=%d hosts=%d", len(report.Local), len(report.Remote), len(report.FederatedHosts))
	}
	if !containsString(report.Local[0].Groups, "session:host-a/"+report.Local[0].LocalID) {
		t.Fatalf("local private groups = %+v", report.Local)
	}
	if report.Remote[0].Kind != "lane" && report.Remote[1].Kind != "lane" {
		t.Fatalf("remote lane classification = %+v", report.Remote)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("operator roster leaked sensitive catalog content: %s", body)
	}
}

func TestOperatorRosterHumanAndJSONSurfaces(t *testing.T) {
	report := operatorRosterReport{
		Schema: operatorRosterSchema,
		Host:   operatorRosterHost{ID: "host-a", Name: "workstation-a", State: "running", Generation: 3, HubConfigured: true},
		Local: []operatorRosterEntry{{
			Kind: "peer", Scope: "local", ID: "peer", LocalID: "peer", NativeSessionID: "native",
			Name: "reviewer", HostID: "host-a", HostName: "workstation-a", Product: "codex",
			State: "attached", Live: true, PermissionMode: "bypassPermissions",
			Groups: []string{"project", "session:host-a/peer"},
		}},
		FederatedHosts: []federationpkg.Host{}, Remote: []operatorRosterEntry{},
	}
	var human bytes.Buffer
	if err := writeOperatorRoster(&human, report); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"LOCAL REGISTRATIONS (1)", "reviewer", "bypassPermissions", "session:host-a/peer", "FEDERATED HOSTS (0)", "REMOTE REGISTRATIONS (0)"} {
		if !strings.Contains(human.String(), value) {
			t.Fatalf("human roster omitted %q:\n%s", value, human.String())
		}
	}

	stateRoot := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{
		StateRoot: stateRoot,
		Handler: func(_ context.Context, request daemonpkg.ControlRequest) (json.RawMessage, error) {
			if request.Operation != "roster" {
				t.Fatalf("operator request = %+v", request)
			}
			return json.Marshal(report)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	invocation := clihelp.Invocation{Command: "roster", Arguments: []string{"--state-root", stateRoot, "--json"}}
	var output bytes.Buffer
	if err := runRoster(context.Background(), invocation, &output); err != nil {
		t.Fatal(err)
	}
	var decoded operatorRosterReport
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || decoded.Schema != operatorRosterSchema {
		t.Fatalf("JSON roster = %s, %v", output.String(), err)
	}
}
