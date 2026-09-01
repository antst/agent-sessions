package main

import (
	"context"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func TestMessagingToolsAddressBusyLaneByEveryAdvertisedIdentity(t *testing.T) {
	for _, target := range []string{"worker", "lane-id", "native-id"} {
		t.Run(target, func(t *testing.T) {
			runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			snapshot, err := runtime.State().Read()
			if err != nil {
				t.Fatal(err)
			}
			catalog := snapshot.Catalog
			catalog.Host.Host = "host-a"
			catalog.Attachments["parent"] = daemonpkg.ManagedAttachment{
				ID: "parent", Product: "codex", NativeSessionID: "native-parent", Name: "parent",
				Cwd: "/work", PermissionMode: "bypassPermissions",
				State: "attached", DaemonGeneration: catalog.Host.Generation,
			}
			catalog.Lanes["lane-id"] = daemonpkg.Lane{
				ID: "lane-id", Product: "qwen", NativeSessionID: "native-id", Name: "worker",
				ParentAttachmentID: "parent", Cwd: "/work",
				Groups: []string{"session:host-a/parent", "session:host-a/lane-id"}, State: "running",
			}
			if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
				t.Fatal(err)
			}
			coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
			coordinator.lanesLoaded = true
			coordinator.lanes["lane-id"] = &laneActor{
				id: "lane-id", product: "qwen", nativeID: "native-id", name: "worker",
				parentID: "parent", cwd: "/work",
				groups:     []string{"session:host-a/parent", "session:host-a/lane-id"},
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
			if _, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "send_message", map[string]any{
				"target": target, "message": "mid-flight correction",
			}); err != nil {
				t.Fatalf("send to advertised target %q: %v", target, err)
			}
			if got := coordinator.lanes["lane-id"].pending; len(got) != 1 {
				t.Fatalf("pending lane messages = %#v", got)
			}
		})
	}
}

func TestUnifiedRenameSessionChangesPublicAttachmentName(t *testing.T) {
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Attachments["peer"] = daemonpkg.ManagedAttachment{
		ID: "peer", Product: "claude", NativeSessionID: "native", Name: "misspelled",
		State: "attached", DaemonGeneration: catalog.Host.Generation,
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), shortDaemonTestRoot(t))
	result, err := coordinator.callLocalTool(context.Background(), runtime, "peer", "rename_session", map[string]any{
		"name": "Builder Corrected",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Data.(map[string]any)["name"] != "Builder-Corrected" {
		t.Fatalf("rename result = %#v", result.Data)
	}
	attachment, active, err := runtime.Attachments().ActiveAttachment("peer")
	if err != nil || !active || attachment.Name != "Builder-Corrected" {
		t.Fatalf("renamed attachment = %+v active=%v err=%v", attachment, active, err)
	}
}
