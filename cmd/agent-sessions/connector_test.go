package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestAutomaticConnectorProductUsesManagedEnvironmentAndCodexFallback(t *testing.T) {
	for _, test := range []struct {
		name, product, grokSession, want string
	}{
		{name: "codex fallback", want: "codex"},
		{name: "codex explicit environment", product: "codex", want: "codex"},
		{name: "claude", product: "claude", want: "claude"},
		{name: "qwen", product: "qwen", want: "qwen"},
		{name: "grok product", product: "grok", want: "grok"},
		{name: "grok private launch", grokSession: "session", want: "grok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(name string) string {
				switch name {
				case "AGENT_SESSIONS_PRODUCT":
					return test.product
				case "AGENT_SESSIONS_GROK_SESSION_ID":
					return test.grokSession
				default:
					return ""
				}
			}
			got, err := resolveConnectorProduct("auto", getenv)
			if err != nil || got != test.want {
				t.Fatalf("resolve automatic connector = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	if _, err := resolveConnectorProduct("auto", func(string) string { return "imaginary" }); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("invalid automatic connector error = %v", err)
	}
}

func TestGrokConnectorAcceptsExactTUIOrPrivateLeaderAncestry(t *testing.T) {
	owner := procinfo.Identity{PID: 101, Start: "owner"}
	leader := procinfo.Identity{PID: 102, Start: "leader"}
	caller := procinfo.Identity{PID: 103, Start: "connector"}
	attachment := daemonpkg.ManagedAttachment{
		Product:  "grok",
		Evidence: daemonpkg.NativeEvidence{Process: owner, Ancestry: []procinfo.Identity{leader}},
	}
	for _, ancestry := range [][]procinfo.Identity{{owner}, {leader}, {caller, owner}, {caller, leader}} {
		if !connectorAncestryMatches(attachment, caller, ancestry) {
			t.Fatalf("Grok connector ancestry %+v was rejected", ancestry)
		}
	}
	if connectorAncestryMatches(attachment, caller, []procinfo.Identity{{PID: 104, Start: "unrelated"}}) {
		t.Fatal("unrelated Grok connector ancestry was accepted")
	}
}

func TestConnectorAttestsDaemonLaneFromInheritedCapability(t *testing.T) {
	root := t.TempDir()
	state, err := daemonpkg.OpenState(root, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.Read()
	if err != nil {
		t.Fatal(err)
	}
	capability := "one-turn-lane-capability"
	catalog := snapshot.Catalog
	catalog.Lanes["lane"] = daemonpkg.Lane{
		ID: "lane", Product: "qwen", NativeSessionID: "native", State: "running",
		CapabilityHash: daemonpkg.CapabilityDigest(capability),
	}
	if _, err := state.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_SESSIONS_SESSION_ID", "lane")
	t.Setenv("AGENT_SESSIONS_LANE_CAPABILITY", capability)
	attestation, err := attestConnectorFromState(root, "qwen", nil, os.Getpid())
	if err != nil || attestation.AttachmentID != "lane" || attestation.Capability != capability || attestation.Evidence.ThreadID != "native" {
		t.Fatalf("lane attestation = %+v, %v", attestation, err)
	}
	t.Setenv("AGENT_SESSIONS_LANE_CAPABILITY", "forged")
	if _, err := attestConnectorFromState(root, "qwen", nil, os.Getpid()); !errors.Is(err, bridge.ErrConnectorInactive) {
		t.Fatalf("forged lane attestation error = %v", err)
	}
}

func TestCodexConnectorAttestsDaemonLaneFromNativeThreadMetadata(t *testing.T) {
	root := t.TempDir()
	state, err := daemonpkg.OpenState(root, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.Read()
	if err != nil {
		t.Fatal(err)
	}
	threadID := "01a04e84-e0fc-7ff0-98cd-7dde31af5a79"
	catalog := snapshot.Catalog
	catalog.Lanes["stable-lane"] = daemonpkg.Lane{
		ID: "stable-lane", Product: "codex", NativeSessionID: threadID, State: "running",
		CapabilityHash: daemonpkg.CapabilityDigest("app-server-cannot-inherit-this"),
	}
	if _, err := state.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"_meta": map[string]any{
		"threadId":              threadID,
		"x-codex-turn-metadata": map[string]any{"session_id": threadID, "thread_id": threadID},
	}})
	attestation, err := attestConnectorFromState(root, "codex", params, os.Getpid())
	if err != nil || attestation.AttachmentID != "stable-lane" || attestation.Capability != "" ||
		attestation.Evidence.ThreadID != threadID {
		t.Fatalf("Codex lane attestation = %+v, %v", attestation, err)
	}
}

func TestConnectorRetriesOnlyARecorroboratingAttachmentGeneration(t *testing.T) {
	root := t.TempDir()
	state, err := daemonpkg.OpenState(root, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	caller, ancestry, err := bridge.CaptureNativeAncestry(os.Getpid(), 32)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Host.Generation = 2
	catalog.Attachments["qwen-peer"] = daemonpkg.ManagedAttachment{
		ID: "qwen-peer", Product: "qwen", State: "attached", DaemonGeneration: 1,
		Evidence: daemonpkg.NativeEvidence{Process: caller, Ancestry: ancestry},
	}
	if _, err := state.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	if _, err := attestConnectorFromState(root, "qwen", nil, os.Getpid()); !errors.Is(err, errConnectorGenerationRecovering) {
		t.Fatalf("recovering attestation error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, attestErr := attestConnectorWithRecovery(context.Background(), root, "qwen", nil, os.Getpid())
		done <- attestErr
	}()
	time.Sleep(40 * time.Millisecond)
	snapshot, err = state.Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog = snapshot.Catalog
	attachment := catalog.Attachments["qwen-peer"]
	attachment.DaemonGeneration = catalog.Host.Generation
	catalog.Attachments[attachment.ID] = attachment
	if _, err := state.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("connector recovery retry = %v", err)
	}
	if _, err := attestConnectorFromState(root, "grok", nil, os.Getpid()); !errors.Is(err, bridge.ErrConnectorInactive) {
		t.Fatalf("bare connector error = %v", err)
	}
}
