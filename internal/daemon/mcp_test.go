package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestMCPServiceOwnsToolInventoryAndBareSessionDecision(t *testing.T) {
	runtime := newWorkflowTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := runtime.mcpService()
	if service == nil {
		t.Fatal("runtime did not recover the MCP authority")
	}

	initialized := service.Forward(context.Background(), controlPrincipal{Role: controlRoleConnector, Product: "qwen"}, MCPForwardRequest{
		Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2025-06-18"}`),
	})
	if initialized.Error != nil || !strings.Contains(string(initialized.Result), `"name":"agent-sessions"`) {
		t.Fatalf("initialize = %#v", initialized)
	}
	listed := service.Forward(context.Background(), controlPrincipal{Role: controlRoleConnector, Product: "qwen"}, MCPForwardRequest{Method: "tools/list"})
	if listed.Error != nil {
		t.Fatalf("tools/list = %#v", listed)
	}
	for _, name := range []string{"list_peers", "send_message", "broadcast", "check_inbox", "identity", "rename_session", "lane"} {
		if !strings.Contains(string(listed.Result), `"name":"`+name+`"`) {
			t.Errorf("tools/list omits %q: %s", name, listed.Result)
		}
	}
	called := service.Forward(context.Background(), controlPrincipal{Role: controlRoleConnector, Product: "qwen"}, MCPForwardRequest{
		Method: "tools/call", Params: json.RawMessage(`{"name":"list_peers","arguments":{}}`),
	})
	if called.Error != nil || !strings.Contains(string(called.Result), "inactive outside an attested peer session") ||
		!strings.Contains(string(called.Result), `"isError":true`) {
		t.Fatalf("bare tools/call = %#v", called)
	}
}

func TestMCPServiceRoutesDiscoveryAndDeliveryThroughDaemonAuthorities(t *testing.T) {
	runtime := newWorkflowTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	source := attachWorkflowMCPPeer(t, runtime, "codex", "source", "source-session", []string{"peer-dev"})
	target := attachWorkflowMCPPeer(t, runtime, "claude", "target", "target-session", []string{"peer-dev"})
	principal := controlPrincipal{
		Role: controlRoleConnector, Product: source.Product, AttachmentID: source.AttachmentID,
		SessionID: source.SessionID, Attested: true,
	}

	discovered := runtime.mcpService().Forward(context.Background(), principal, MCPForwardRequest{
		Method: "tools/call", Params: json.RawMessage(`{"name":"list_peers","arguments":{}}`),
	})
	if discovered.Error != nil || !strings.Contains(string(discovered.Result), target.SessionID) ||
		!strings.Contains(string(discovered.Result), `"isError":false`) {
		t.Fatalf("list_peers = %#v", discovered)
	}

	sent := runtime.mcpService().Forward(context.Background(), principal, MCPForwardRequest{
		Method: "tools/call", Params: json.RawMessage(`{"name":"send_message","arguments":{"target":"workflow-host/target-session","message":"hello"}}`),
	})
	if sent.Error != nil || !strings.Contains(string(sent.Result), `"state":"delivered"`) ||
		!strings.Contains(string(sent.Result), `"isError":false`) {
		t.Fatalf("send_message = %#v", sent)
	}
}

func TestForegroundConnectorHelloDistinguishesBareFromAttested(t *testing.T) {
	runtime := newWorkflowTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	authorize := authorizeForegroundHello(runtime)
	bare, failure := authorize(context.Background(), controlPeerEvidence{UID: os.Getuid()}, controlHello{
		Role: controlRoleConnector, Product: "grok",
	})
	if failure != nil || bare.Attested || bare.Product != "grok" {
		t.Fatalf("bare connector = %#v, failure=%#v", bare, failure)
	}
	if _, failure := authorize(context.Background(), controlPeerEvidence{UID: os.Getuid()}, controlHello{
		Role: controlRoleConnector, Product: "grok", AttachmentID: "partial",
	}); failure == nil {
		t.Fatal("partial connector attestation was accepted as bare")
	}
}

func TestPreparedConnectorWithoutLateSessionRemainsSelecting(t *testing.T) {
	runtime := newWorkflowTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	prepared, capability, err := runtime.attachmentRegistry().Prepare(context.Background(), AttachmentPrepareRequest{
		Product: "claude", Kind: "interactive", Cwd: "/workspace", Name: "selecting", Groups: []string{"peer-dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	principal, failure := authorizeForegroundHello(runtime)(context.Background(), controlPeerEvidence{UID: os.Getuid()}, controlHello{
		Role: controlRoleConnector, Product: "claude", AttachmentID: prepared.AttachmentID, Capability: capability,
	})
	if failure != nil || principal.AttachmentID != prepared.AttachmentID || principal.Attested || principal.SessionID != "" {
		t.Fatalf("selecting principal = %#v, failure=%#v", principal, failure)
	}
	result := runtime.mcpService().Forward(context.Background(), principal, MCPForwardRequest{
		Method: "tools/call", Params: json.RawMessage(`{"name":"identity","arguments":{}}`),
	})
	if result.Error != nil || !strings.Contains(string(result.Result), "inactive outside an attested peer session") {
		t.Fatalf("selecting tool call = %#v", result)
	}
}

func TestBareConnectorCanInitializeButCannotAcquirePeerAuthority(t *testing.T) {
	runtime := newWorkflowTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := newControlTestServer(controlServerConfig{
		Generation: runtime.Generation(), RuntimeVersion: "test",
		AuthorizeHello: authorizeForegroundHello(runtime), Dispatch: runtimeControlDispatch(runtime),
	})
	client, serverErr := startControlTestSession(t, server)
	writeControlTestFrame(t, client, map[string]any{
		"type": "hello", "version": 1, "request_id": "bare-hello", "role": "connector", "product": "claude",
	})
	if hello := readControlTestFrame(t, client); hello["type"] != "hello.result" || hello["attachment_id"] != nil {
		t.Fatalf("bare hello = %#v", hello)
	}
	writeControlTestFrame(t, client, map[string]any{
		"type": "request", "version": 1, "request_id": "bare-call", "operation": "mcp.forward",
		"expected_generation": runtime.Generation(),
		"payload":             map[string]any{"method": "tools/call", "params": map[string]any{"name": "identity", "arguments": map[string]any{}}},
	})
	response := readControlTestFrame(t, client)
	body, _ := json.Marshal(response)
	accepted, _ := response["accepted"].(bool)
	if !accepted || !strings.Contains(string(body), "inactive outside an attested peer session") {
		t.Fatalf("bare tools/call response = %s", body)
	}
	_ = client.Close()
	if err := waitControlTestServer(t, serverErr); err != nil {
		t.Fatal(err)
	}
}

func attachWorkflowMCPPeer(t *testing.T, runtime *Runtime, product, name, sessionID string, groups []string) AttachmentRecord {
	t.Helper()
	registry := runtime.attachmentRegistry()
	prepared, capability, err := registry.Prepare(context.Background(), AttachmentPrepareRequest{
		Product: product, Kind: "interactive", Cwd: "/workspace", Name: name, Groups: groups,
		ExpectedNativeActor: map[string]any{"pid": 71},
	})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := registry.Adopt(context.Background(), AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: sessionID,
		NativeActor: map[string]any{"pid": 71},
	})
	if err != nil {
		t.Fatal(err)
	}
	return attached
}
