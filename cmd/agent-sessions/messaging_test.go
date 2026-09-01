package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
)

func TestMCPStableOperationRetryUsesOneLaneReceiptAndNativeTurn(t *testing.T) {
	root := shortDaemonTestRoot(t)
	operationID := "mcp-stable-lane-operation"
	receiptID := laneCommandInputID(daemonpkg.ControlRequest{IdempotencyKey: operationID})
	laneID := laneIDForInitialReceipt(receiptID)
	claudeBin := filepath.Join(root, "claude-mcp-stable")
	promptLog := filepath.Join(root, "mcp-stable-prompts.log")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  --version) printf '2.1.233\n'; exit 0 ;;
  'auth status --json') printf '%%s\n' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}'; exit 0 ;;
esac
last=
for argument do last="$argument"; done
printf '%%s\n' "$last" >> "$MCP_STABLE_PROMPT_LOG"
printf '%%s\n' '{"session_id":"%s","result":"mcp result"}'
`, laneID)
	if err := os.WriteFile(claudeBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	t.Setenv("MCP_STABLE_PROMPT_LOG", promptLog)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Attachments["parent"] = daemonpkg.ManagedAttachment{
		ID: "parent", Product: "codex", NativeSessionID: "native-parent", Cwd: root,
		PermissionMode: "bypassPermissions", State: "attached", DaemonGeneration: catalog.Host.Generation,
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	coordinator := newHostCoordinator(context.Background(), root)
	args := map[string]any{
		"product": "claude", "command": "run", "arguments": []string{"--name", "mcp-worker", "--persistent"},
		"input": "mcp stable prompt",
	}
	first, err := coordinator.callLocalToolWithID(context.Background(), runtime, "parent", "lane", args, operationID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.callLocalToolWithID(context.Background(), runtime, "parent", "lane", args, operationID)
	if err != nil {
		t.Fatal(err)
	}
	firstData, secondData := first.Data.(map[string]any), second.Data.(map[string]any)
	firstReceiptID, _ := firstData["receipt_id"].(string)
	if !strings.HasPrefix(firstReceiptID, receiptID+"-") || secondData["receipt_id"] != firstReceiptID ||
		firstData["turn_id"] != secondData["turn_id"] {
		t.Fatalf("MCP retry results first=%#v second=%#v", firstData, secondData)
	}
	prompts, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimSpace(string(prompts)), "\n") != 0 || strings.TrimSpace(string(prompts)) != "mcp stable prompt" {
		t.Fatalf("MCP retry duplicated native input: %q", prompts)
	}
}

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
				ID: "parent", Product: "codex", NativeSessionID: "native-parent",
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
			sent, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "send_message", map[string]any{
				"target": target, "message": "mid-flight correction",
			})
			if err != nil {
				t.Fatalf("send to advertised target %q: %v", target, err)
			}
			deliveries := sent.Data.(map[string]any)["deliveries"].([]federationpkg.DeliveryResult)
			if len(deliveries) != 1 || deliveries[0].DeliveryID == "" || deliveries[0].Status != "accepted" {
				t.Fatalf("live lane delivery result = %+v", deliveries)
			}
			after, err := runtime.State().Read()
			if err != nil {
				t.Fatal(err)
			}
			queued := 0
			for _, receipt := range after.Catalog.LaneInputs {
				if receipt.LaneID == "lane-id" && receipt.State == daemonpkg.ReceiptQueued {
					queued++
				}
			}
			if queued != 1 {
				t.Fatalf("durable queued lane receipts = %d", queued)
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
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Attachments["peer"] = daemonpkg.ManagedAttachment{
		ID: "peer", Product: "claude", NativeSessionID: "native",
		State: "attached", DaemonGeneration: catalog.Host.Generation,
	}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
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
	encoded, err := json.Marshal(current.Catalog.Attachments["peer"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"name"`) || strings.Contains(string(encoded), `"native_name"`) {
		t.Fatalf("attachment retained a durable title: %s", encoded)
	}
}
