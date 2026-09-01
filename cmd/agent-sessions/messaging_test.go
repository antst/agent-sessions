package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func TestMessagingToolsRejectBusyLaneByEveryAdvertisedIdentity(t *testing.T) {
	for _, target := range []string{"worker", "lane-id", "native-id"} {
		t.Run(target, func(t *testing.T) {
			runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			parentGroup := "session:" + runtime.HostID() + "/parent"
			laneGroup := "session:" + runtime.HostID() + "/lane-id"
			activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
				ID: "parent", Product: "codex", NativeSessionID: "native-parent",
				Cwd: "/work", PermissionMode: "bypassPermissions",
			})
			coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
			coordinator.lanesLoaded = true
			coordinator.lanes["lane-id"] = &laneActor{
				id: "lane-id", product: "qwen", nativeID: "native-id", name: "worker",
				parentID: "parent", cwd: "/work",
				groups:     []string{parentGroup, laneGroup},
				permission: "default", state: "running",
			}

			listed, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "list_peers", map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			peers := listed.Data.(map[string]any)["peers"].([]map[string]any)
			if len(peers) != 1 || peers[0]["id"] != "lane-id" || peers[0]["session_id"] != "native-id" || peers[0]["name"] != "worker" {
				t.Fatalf("advertised peers = %#v", peers)
			}
			_, err = coordinator.callLocalTool(context.Background(), runtime, "parent", "send_message", map[string]any{
				"target": target, "message": "mid-flight correction",
			})
			if err == nil || !strings.Contains(err.Error(), "did not accept") {
				t.Fatalf("busy lane delivery error = %v", err)
			}
		})
	}
}

func TestUnifiedRenameNeverCreatesADurableDaemonAlias(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	activateTestAttachment(t, runtime, daemonpkg.ManagedAttachment{
		ID: "peer", Product: "claude", NativeSessionID: "native",
	})
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	_, err = coordinator.callLocalTool(context.Background(), runtime, "peer", "rename_session", map[string]any{
		"name": "Builder Corrected",
	})
	if err == nil || !strings.Contains(err.Error(), "native rename driver") {
		t.Fatalf("rename error = %v", err)
	}
	current, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(current.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"attachments"`) || strings.Contains(string(encoded), `"name"`) {
		t.Fatalf("live peer leaked into durable catalog: %s", encoded)
	}
}
