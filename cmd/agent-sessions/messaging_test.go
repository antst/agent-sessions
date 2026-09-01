package main

import (
	"context"
	"encoding/json"
	"errors"
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
		ID: "parent", Product: "codex", NativeSessionID: "native-parent", Name: "parent", Cwd: root,
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

func TestLaneReceiptProjectionReadFailurePreservesAcceptedDelivery(t *testing.T) {
	accepted := federationpkg.AgentFrameResult{
		Version: federationpkg.AgentFrameVersion,
		Type:    "send.result",
		Deliveries: []federationpkg.DeliveryResult{{
			Target: "lane-id", DeliveryID: "stable-delivery", Status: "accepted",
		}},
	}
	got := projectLaneDeliveryReceipts(accepted, daemonpkg.StateSnapshot{}, errors.New("projection read failed"))
	if len(got.Deliveries) != 1 || got.Deliveries[0].Status != "accepted" ||
		got.Deliveries[0].DeliveryID != "stable-delivery" || got.Deliveries[0].ReceiptID != "" {
		t.Fatalf("post-acceptance projection failure changed result: %+v", got)
	}
}

func TestFederatedLaneReceiptAcknowledgementProjectsAtSource(t *testing.T) {
	result := federationpkg.AgentFrameResult{Deliveries: []federationpkg.DeliveryResult{{
		Target: "host-b/lane", DeliveryID: "delivery-id", Status: "accepted",
	}}}
	got := projectFederatedLaneReceipts(result, map[string]federatedLaneReceiptAck{
		"delivery-id": {ReceiptID: "destination-receipt", ReceiptSequence: 9},
	})
	if got.Deliveries[0].ReceiptID != "destination-receipt" || got.Deliveries[0].ReceiptSequence != 9 {
		t.Fatalf("federated receipt projection = %+v", got.Deliveries)
	}
}

func TestFederatedDestinationReceiptProjectionSupportsIdempotentRetry(t *testing.T) {
	deliveries := []federationpkg.DeliveryResult{{
		Target: "lane-id", DeliveryID: "destination-delivery", Status: "accepted",
	}}
	snapshot := daemonpkg.StateSnapshot{Catalog: daemonpkg.Catalog{LaneInputs: map[string]daemonpkg.LaneInputReceipt{
		"destination-delivery": {
			ReceiptID: "destination-delivery", LaneID: "lane-id", Sequence: 4, State: daemonpkg.ReceiptQueued,
		},
	}}}
	data := projectFederatedDestinationReceipt(deliveries, snapshot, &laneActor{id: "lane-id"})
	var got federatedLaneReceiptAck
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ReceiptID != "destination-delivery" || got.ReceiptSequence != 4 {
		t.Fatalf("idempotent destination receipt = %+v", got)
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
			sent, err := coordinator.callLocalTool(context.Background(), runtime, "parent", "send_message", map[string]any{
				"target": target, "message": "mid-flight correction",
			})
			if err != nil {
				t.Fatalf("send to advertised target %q: %v", target, err)
			}
			deliveries := sent.Data.(map[string]any)["deliveries"].([]federationpkg.DeliveryResult)
			if len(deliveries) != 1 || deliveries[0].DeliveryID == "" || deliveries[0].ReceiptID != deliveries[0].DeliveryID || deliveries[0].ReceiptSequence != 1 {
				t.Fatalf("lane delivery receipt projection = %+v", deliveries)
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
