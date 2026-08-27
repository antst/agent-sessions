package daemon

import (
	"context"
	"encoding/json"
	"net"
	"testing"
)

func TestLocalControlLauncherPreparesThroughOneCorrelatedExchange(t *testing.T) {
	server := newControlTestServer(controlServerConfig{
		Generation: 17, RuntimeVersion: "0.3.0",
		AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
			return controlPrincipal{Role: hello.Role}, nil
		},
		Dispatch: func(_ context.Context, principal controlPrincipal, request controlRequest) (controlDispatchResult, *controlError) {
			if principal.Role != controlRoleLauncher || request.Operation != "attachment.prepare" {
				t.Fatalf("dispatch = role %q operation %q", principal.Role, request.Operation)
			}
			return controlDispatchResult{ResourceRevision: "1", Result: json.RawMessage(`{"attachment":{"attachment_id":"attachment-1"},"capability":"capability-1"}`)}, nil
		},
	})
	client, serverErr := startControlTestSession(t, server)
	result, err := queryLocalControlConnection(context.Background(), client, LocalControlIdentity{Role: LocalControlLauncher}, "attachment.prepare", AttachmentPrepareRequest{Product: "qwen"})
	if err != nil {
		t.Fatalf("query launcher: %v", err)
	}
	if result.Generation != 17 || result.ResourceRevision != "1" {
		t.Fatalf("result = %#v", result)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := waitControlTestServer(t, serverErr); err != nil {
		t.Fatalf("control server: %v", err)
	}
}

func TestLocalControlConnectorSendsAttestationOnlyInHello(t *testing.T) {
	server := newControlTestServer(controlServerConfig{
		Generation: 19, RuntimeVersion: "0.3.0",
		AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
			if hello.Capability != "raw-capability" || hello.SessionID != "native-1" || hello.NativeActor["pid"] != float64(31) {
				t.Fatalf("connector hello = %#v", hello)
			}
			return controlPrincipal{Role: hello.Role, Product: hello.Product, AttachmentID: hello.AttachmentID, SessionID: hello.SessionID}, nil
		},
		Dispatch: func(_ context.Context, _ controlPrincipal, _ controlRequest) (controlDispatchResult, *controlError) {
			return controlDispatchResult{Result: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	client, serverErr := startControlTestSession(t, server)
	_, err := queryLocalControlConnection(context.Background(), client, LocalControlIdentity{
		Role: LocalControlConnector, Product: "qwen", AttachmentID: "attachment-1",
		SessionID: "native-1", Capability: "raw-capability", NativeActor: map[string]any{"pid": 31},
	}, "peer.identity", map[string]any{})
	if err != nil {
		t.Fatalf("query connector: %v", err)
	}
	_ = client.Close()
	if err := waitControlTestServer(t, serverErr); err != nil {
		t.Fatalf("control server: %v", err)
	}
}

func TestLocalControlRejectsUnknownRoleBeforeWriting(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	if _, err := queryLocalControlConnection(context.Background(), client, LocalControlIdentity{Role: "daemon-owner"}, "runtime.status", nil); err == nil {
		t.Fatal("unknown local role was accepted")
	}
}
