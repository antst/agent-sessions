package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/testutil"
)

const controlTestTimeout = 2 * time.Second

func TestControlSessionRequiresCompatibleHelloFirst(t *testing.T) {
	tests := []struct {
		name  string
		frame map[string]any
	}{
		{
			name: "request before hello",
			frame: map[string]any{
				"type": "request", "version": 1, "request_id": "request-first",
				"operation": "runtime.status", "expected_generation": 42, "payload": map[string]any{},
			},
		},
		{
			name: "incompatible version",
			frame: map[string]any{
				"type": "hello", "version": 2, "request_id": "wrong-version", "role": "admin",
			},
		},
		{
			name: "unknown role",
			frame: map[string]any{
				"type": "hello", "version": 1, "request_id": "wrong-role", "role": "model-admin",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var authorized atomic.Int32
			server := newControlTestServer(controlServerConfig{
				Generation:     42,
				RuntimeVersion: "0.3.0",
				AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
					authorized.Add(1)
					return controlPrincipal{Role: hello.Role}, nil
				},
			})
			client, serverErr := startControlTestSession(t, server)
			writeControlTestFrame(t, client, test.frame)
			if err := client.Close(); err != nil {
				t.Fatalf("close client: %v", err)
			}
			if err := waitControlTestServer(t, serverErr); err == nil {
				t.Fatal("invalid first frame was accepted")
			}
			if got := authorized.Load(); got != 0 {
				t.Fatalf("hello authorizer calls = %d, want 0 for a structurally invalid hello", got)
			}
		})
	}
}

func TestControlHelloAndRequestResponseAreNDJSONAndCorrelated(t *testing.T) {
	helloSeen := make(chan controlHello, 1)
	dispatchSeen := make(chan struct {
		principal controlPrincipal
		request   controlRequest
	}, 1)
	server := newControlTestServer(controlServerConfig{
		Generation:     42,
		RuntimeVersion: "0.3.0",
		AuthorizeHello: func(_ context.Context, peer controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
			helloSeen <- hello
			return controlPrincipal{
				Role: controlRoleConnector, Product: "qwen", AttachmentID: "attachment-1",
				SessionID: "native-session-1", Peer: peer,
			}, nil
		},
		Dispatch: func(_ context.Context, principal controlPrincipal, request controlRequest) (controlDispatchResult, *controlError) {
			dispatchSeen <- struct {
				principal controlPrincipal
				request   controlRequest
			}{principal: principal, request: request}
			return controlDispatchResult{
				ResourceRevision: "delivery-revision-8",
				Result:           json.RawMessage(`{"message_id":"message-1","state":"accepted"}`),
			}, nil
		},
	})
	client, serverErr := startControlTestSession(t, server)

	writeControlTestFrame(t, client, map[string]any{
		"type": "hello", "version": 1, "request_id": "hello-1", "role": "connector",
		"product": "qwen", "attachment_id": "attachment-1", "session_id": "native-session-1", "capability": "raw-secret-capability",
	})
	helloResult := readControlTestFrame(t, client)
	wantHello := map[string]any{
		"type": "hello.result", "version": float64(1), "request_id": "hello-1",
		"daemon_generation": float64(42), "runtime_version": "0.3.0", "role": "connector",
		"attachment_id": "attachment-1", "session_id": "native-session-1",
	}
	if !reflect.DeepEqual(helloResult, wantHello) {
		t.Fatalf("hello result = %#v, want %#v", helloResult, wantHello)
	}
	if _, leaked := helloResult["capability"]; leaked {
		t.Fatal("hello result echoed the raw launch capability")
	}
	gotHello := <-helloSeen
	if gotHello.Role != controlRoleConnector || gotHello.Product != "qwen" ||
		gotHello.AttachmentID != "attachment-1" || gotHello.Capability != "raw-secret-capability" {
		t.Fatalf("authorize hello = %#v", gotHello)
	}

	writeControlTestFrame(t, client, map[string]any{
		"type": "request", "version": 1, "request_id": "request-1", "operation": "peer.send",
		"expected_generation": 42, "expected_revision": "peer-revision-7",
		"payload": map[string]any{"message_id": "message-1", "content": "test-only-content"},
	})
	response := readControlTestFrame(t, client)
	if response["type"] != "response" || response["version"] != float64(1) ||
		response["request_id"] != "request-1" || response["operation"] != "peer.send" {
		t.Fatalf("uncorrelated response envelope: %#v", response)
	}
	if response["daemon_generation"] != float64(42) || response["accepted"] != true || //nolint:revive // Dynamic JSON value requires explicit Boolean comparison.
		response["resource_revision"] != "delivery-revision-8" {
		t.Fatalf("response acceptance fields = %#v", response)
	}
	wantResult := map[string]any{"message_id": "message-1", "state": "accepted"}
	if !reflect.DeepEqual(response["result"], wantResult) {
		t.Fatalf("response result = %#v, want %#v", response["result"], wantResult)
	}
	dispatched := <-dispatchSeen
	if dispatched.principal.Role != controlRoleConnector || dispatched.principal.SessionID != "native-session-1" {
		t.Fatalf("dispatch principal = %#v", dispatched.principal)
	}
	if dispatched.request.RequestID != "request-1" || dispatched.request.Operation != "peer.send" ||
		dispatched.request.ExpectedGeneration != 42 || dispatched.request.ExpectedRevision != "peer-revision-7" {
		t.Fatalf("dispatch request = %#v", dispatched.request)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	_ = waitControlTestServer(t, serverErr)
}

func TestControlNotificationsRequireRoleScopedExplicitSubscription(t *testing.T) {
	server := newControlTestServer(controlServerConfig{
		Generation: 42, RuntimeVersion: "0.3.0",
		AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
			return controlPrincipal{Role: hello.Role, Product: hello.Product, AttachmentID: hello.AttachmentID}, nil
		},
	})
	client, serverErr := startControlTestSession(t, server)
	helloControlTestSession(t, client, "connector")
	writeControlTestFrame(t, client, map[string]any{
		"type": "subscribe", "version": 1, "request_id": "subscribe-1",
		"expected_generation": 42, "topics": []string{"lane.notice", "peer.inbox"},
	})
	response := readControlTestFrame(t, client)
	if response["type"] != "subscribe.result" || response["request_id"] != "subscribe-1" ||
		response["daemon_generation"] != float64(42) {
		t.Fatalf("subscription response = %#v", response)
	}
	topics, ok := response["topics"].([]any)
	if !ok || !reflect.DeepEqual(topics, []any{"lane.notice", "peer.inbox"}) {
		t.Fatalf("subscription topics = %#v", response["topics"])
	}
	_ = client.Close()
	_ = waitControlTestServer(t, serverErr)

	adminClient, adminErr := startControlTestSession(t, server)
	helloControlTestSession(t, adminClient, "admin")
	writeControlTestFrame(t, adminClient, map[string]any{
		"type": "subscribe", "version": 1, "request_id": "subscribe-forbidden",
		"expected_generation": 42, "topics": []string{"peer.inbox"},
	})
	assertRejectedControlResponse(t, readControlTestFrame(t, adminClient), "subscribe-forbidden", "subscribe", "topic_forbidden")
	_ = adminClient.Close()
	_ = waitControlTestServer(t, adminErr)
}

func TestControlFrameBoundIsExactlyTwoMiB(t *testing.T) {
	if maxLocalControlFrameBytes != 2*1024*1024 {
		t.Fatalf("maximum local frame = %d, want 2 MiB", maxLocalControlFrameBytes)
	}

	t.Run("boundary accepted", func(t *testing.T) {
		server := newControlTestServer(controlServerConfig{
			Generation:     42,
			RuntimeVersion: "0.3.0",
			AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
				return controlPrincipal{Role: hello.Role}, nil
			},
		})
		client, serverErr := startControlTestSession(t, server)
		body := []byte(`{"type":"hello","version":1,"request_id":"bounded","role":"admin"}`)
		body = append(body, []byte(strings.Repeat(" ", maxLocalControlFrameBytes-len(body)))...)
		writeRawControlTestLine(t, client, body)
		if got := readControlTestFrame(t, client); got["type"] != "hello.result" {
			t.Fatalf("boundary hello response = %#v", got)
		}
		_ = client.Close()
		_ = waitControlTestServer(t, serverErr)
	})

	t.Run("one byte over rejected before authorization", func(t *testing.T) {
		var authorized atomic.Int32
		server := newControlTestServer(controlServerConfig{
			Generation:     42,
			RuntimeVersion: "0.3.0",
			AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
				authorized.Add(1)
				return controlPrincipal{Role: hello.Role}, nil
			},
		})
		client, serverErr := startControlTestSession(t, server)
		body := []byte(`{"type":"hello","version":1,"request_id":"oversized","role":"admin"}`)
		body = append(body, []byte(strings.Repeat(" ", maxLocalControlFrameBytes+1-len(body)))...)
		writeRawControlTestLine(t, client, body)
		_ = client.Close()
		if err := waitControlTestServer(t, serverErr); err == nil {
			t.Fatal("oversized local frame was accepted")
		}
		if got := authorized.Load(); got != 0 {
			t.Fatalf("hello authorizer calls = %d, want 0 for oversized frame", got)
		}
	})
}

func TestControlRejectsObsoleteOrFutureGenerationBeforeDispatch(t *testing.T) {
	for _, generation := range []uint64{41, 43} {
		t.Run(fmt.Sprintf("generation_%d", generation), func(t *testing.T) {
			var dispatches atomic.Int32
			server := newControlTestServer(controlServerConfig{
				Generation:     42,
				RuntimeVersion: "0.3.0",
				AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
					return controlPrincipal{Role: hello.Role}, nil
				},
				Dispatch: func(_ context.Context, _ controlPrincipal, _ controlRequest) (controlDispatchResult, *controlError) {
					dispatches.Add(1)
					return controlDispatchResult{}, nil
				},
			})
			client, serverErr := startControlTestSession(t, server)
			helloControlTestSession(t, client, "admin")
			writeControlTestFrame(t, client, map[string]any{
				"type": "request", "version": 1, "request_id": "generation-check",
				"operation": "runtime.status", "expected_generation": generation, "payload": map[string]any{},
			})
			response := readControlTestFrame(t, client)
			assertRejectedControlResponse(t, response, "generation-check", "runtime.status", "stale_generation")
			if response["daemon_generation"] != float64(42) {
				t.Fatalf("rejection generation = %#v, want 42", response["daemon_generation"])
			}
			if got := dispatches.Load(); got != 0 {
				t.Fatalf("dispatches = %d, want 0 before generation validation", got)
			}
			_ = client.Close()
			_ = waitControlTestServer(t, serverErr)
		})
	}
}

func TestAcceptedControlRequestIDIsAnIdempotencyKey(t *testing.T) {
	var dispatches atomic.Int32
	replay := newTestPersistedControlReplayStore()
	newServer := func(generation uint64) *controlServer {
		return newControlTestServer(controlServerConfig{
			Generation:     generation,
			RuntimeVersion: "0.3.0",
			ReplayStore:    replay,
			AuthorizeHello: func(_ context.Context, peer controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
				return controlPrincipal{
					Role: hello.Role, Product: hello.Product, AttachmentID: hello.AttachmentID,
					SessionID: "session-for-" + hello.AttachmentID, Peer: peer,
				}, nil
			},
			Dispatch: func(_ context.Context, _ controlPrincipal, _ controlRequest) (controlDispatchResult, *controlError) {
				dispatches.Add(1)
				return controlDispatchResult{
					ResourceRevision: "delivery-9",
					Result:           json.RawMessage(`{"state":"accepted"}`),
				}, nil
			},
		})
	}

	firstRequest := map[string]any{
		"type": "request", "version": 1, "request_id": "idempotency-key-1", "operation": "peer.send",
		"expected_generation": 42, "payload": map[string]any{"message_id": "message-1", "content": "hello"},
	}
	first := roundTripControlTestRequest(t, newServer(42), "attachment-1", firstRequest)
	if first["accepted"] != true { //nolint:revive // Dynamic JSON value requires explicit Boolean comparison.
		t.Fatalf("first request was not accepted: %#v", first)
	}

	// A wholly new server and generation must recover the committed response
	// through the persisted seam. expected_generation is deliberately updated:
	// it is not part of the operation/payload idempotency fingerprint.
	retryRequest := map[string]any{
		"type": "request", "version": 1, "request_id": "idempotency-key-1", "operation": "peer.send",
		"expected_generation": 43, "payload": map[string]any{"message_id": "message-1", "content": "hello"},
	}
	secondServer := newServer(43)
	second := roundTripControlTestRequest(t, secondServer, "attachment-1", retryRequest)
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("persisted cross-generation replay = %#v, want original %#v", second, first)
	}
	if got := dispatches.Load(); got != 1 {
		t.Fatalf("dispatches = %d, want one durable acceptance", got)
	}

	changedOperation := map[string]any{
		"type": "request", "version": 1, "request_id": "idempotency-key-1", "operation": "peer.broadcast",
		"expected_generation": 43, "payload": map[string]any{"message_id": "message-1", "content": "hello"},
	}
	operationCollision := roundTripControlTestRequest(t, secondServer, "attachment-1", changedOperation)
	assertRejectedControlResponse(t, operationCollision, "idempotency-key-1", "peer.broadcast", "idempotency_conflict")

	changedPayload := map[string]any{
		"type": "request", "version": 1, "request_id": "idempotency-key-1", "operation": "peer.send",
		"expected_generation": 43, "payload": map[string]any{"message_id": "message-2", "content": "different"},
	}
	payloadCollision := roundTripControlTestRequest(t, secondServer, "attachment-1", changedPayload)
	assertRejectedControlResponse(t, payloadCollision, "idempotency-key-1", "peer.send", "idempotency_conflict")
	if got := dispatches.Load(); got != 1 {
		t.Fatalf("collision dispatches = %d, want original dispatch only", got)
	}

	// The same request ID belongs to a different idempotency namespace for a
	// different exact authorized principal.
	differentPrincipal := roundTripControlTestRequest(t, secondServer, "attachment-2", retryRequest)
	if differentPrincipal["accepted"] != true { //nolint:revive // Dynamic JSON value requires explicit Boolean comparison.
		t.Fatalf("different-principal request was not independently accepted: %#v", differentPrincipal)
	}
	if got := dispatches.Load(); got != 2 {
		t.Fatalf("different-principal dispatches = %d, want 2", got)
	}
	if got := replay.len(); got != 2 {
		t.Fatalf("persisted replay records = %d, want one per exact principal", got)
	}
}

func TestObserveControlPeerUsesKernelUnixCredentials(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local control peer credentials are supported on Linux and Darwin")
	}

	root := testutil.ShortSocketRoot(t, "as-control-peer-", "daemon.sock")
	endpoint := filepath.Join(root, "daemon.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatalf("listen on test control socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	observed := make(chan controlPeerEvidence, 1)
	observeErr := make(chan error, 1)
	go func() {
		accepted, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			observeErr <- acceptErr
			return
		}
		defer func() { _ = accepted.Close() }()
		peer, peerErr := observeControlPeer(accepted)
		if peerErr != nil {
			observeErr <- peerErr
			return
		}
		observed <- peer
	}()

	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatalf("dial test control socket: %v", err)
	}
	defer func() { _ = client.Close() }()

	select {
	case err := <-observeErr:
		t.Fatalf("observe accepted control peer: %v", err)
	case peer := <-observed:
		if peer.UID != os.Getuid() {
			t.Fatalf("observed UID = %d, want current UID %d", peer.UID, os.Getuid())
		}
		if peer.PID != os.Getpid() {
			t.Fatalf("observed PID = %d, want dialing PID %d", peer.PID, os.Getpid())
		}
		if peer.ProcStart == "" || peer.StrongStart == "" {
			t.Fatalf("observed process start evidence is incomplete: %#v", peer)
		}
	case <-time.After(controlTestTimeout):
		t.Fatal("timed out observing kernel Unix peer credentials")
	}
}

func TestControlHelloUsesKernelObservedPeerEvidence(t *testing.T) {
	t.Run("observed UID PID and start evidence reaches the authorized principal", func(t *testing.T) {
		observed := controlPeerEvidence{
			UID: 1000, PID: 4242, ProcStart: "kernel-proc-start", StrongStart: "kernel-strong-start",
		}
		authorizedPeer := make(chan controlPeerEvidence, 1)
		dispatchedPrincipal := make(chan controlPrincipal, 1)
		server := newControlServer(controlServerConfig{
			Generation:     42,
			RuntimeVersion: "0.3.0",
			OwnerUID:       1000,
			ObservePeer: func(conn net.Conn) (controlPeerEvidence, error) {
				if conn == nil {
					return controlPeerEvidence{}, errors.New("nil accepted connection")
				}
				return observed, nil
			},
			AuthorizeHello: func(_ context.Context, peer controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
				authorizedPeer <- peer
				return controlPrincipal{
					Role: hello.Role, Product: hello.Product, AttachmentID: hello.AttachmentID,
					SessionID: "native-session-1", Peer: peer,
				}, nil
			},
			Dispatch: func(_ context.Context, principal controlPrincipal, _ controlRequest) (controlDispatchResult, *controlError) {
				dispatchedPrincipal <- principal
				return controlDispatchResult{Result: json.RawMessage(`{"healthy":true}`)}, nil
			},
		})
		client, serverErr := startControlTestSession(t, server)
		writeControlTestFrame(t, client, map[string]any{
			"type": "hello", "version": 1, "request_id": "observed-peer", "role": "admin",
		})
		if result := readControlTestFrame(t, client); result["type"] != "hello.result" || result["role"] != "admin" {
			t.Fatalf("hello result = %#v", result)
		}
		if got := <-authorizedPeer; !reflect.DeepEqual(got, observed) {
			t.Fatalf("hello authorizer peer = %#v, want kernel-observed %#v", got, observed)
		}
		writeControlTestFrame(t, client, map[string]any{
			"type": "request", "version": 1, "request_id": "observed-status",
			"operation": "runtime.status", "expected_generation": 42, "payload": map[string]any{},
		})
		if response := readControlTestFrame(t, client); response["accepted"] != true { //nolint:revive // Dynamic JSON value requires explicit Boolean comparison.
			t.Fatalf("authorized request response = %#v", response)
		}
		if got := <-dispatchedPrincipal; !reflect.DeepEqual(got.Peer, observed) {
			t.Fatalf("dispatch principal peer = %#v, want observed %#v", got.Peer, observed)
		}
		_ = client.Close()
		_ = waitControlTestServer(t, serverErr)
	})

	t.Run("claimed identity fields are rejected or ignored", func(t *testing.T) {
		observed := controlPeerEvidence{
			UID: 1000, PID: 4242, ProcStart: "kernel-proc-start", StrongStart: "kernel-strong-start",
		}
		authorizedPeer := make(chan controlPeerEvidence, 1)
		server := newControlServer(controlServerConfig{
			Generation:     42,
			RuntimeVersion: "0.3.0",
			OwnerUID:       1000,
			ObservePeer: func(net.Conn) (controlPeerEvidence, error) {
				return observed, nil
			},
			AuthorizeHello: func(_ context.Context, peer controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
				authorizedPeer <- peer
				return controlPrincipal{Role: hello.Role, Peer: peer}, nil
			},
		})
		client, serverErr := startControlTestSession(t, server)
		writeControlTestFrame(t, client, map[string]any{
			"type": "hello", "version": 1, "request_id": "forged-evidence", "role": "admin",
			// These fields are outside the hello contract. An implementation may
			// reject them or ignore them, but it must never treat them as evidence.
			"uid": 0, "pid": 1, "proc_start": "claimed-start", "strong_start": "claimed-strong",
		})
		_ = client.Close()
		_ = waitControlTestServer(t, serverErr)
		select {
		case got := <-authorizedPeer:
			if !reflect.DeepEqual(got, observed) {
				t.Fatalf("claimed identity replaced observed evidence: got %#v, want %#v", got, observed)
			}
		default:
			// Strict unknown-field rejection is also fail-closed.
		}
	})

	t.Run("claimed owner UID cannot bypass observed different user", func(t *testing.T) {
		var observations atomic.Int32
		var authorizations atomic.Int32
		server := newControlServer(controlServerConfig{
			Generation:     42,
			RuntimeVersion: "0.3.0",
			OwnerUID:       1000,
			ObservePeer: func(net.Conn) (controlPeerEvidence, error) {
				observations.Add(1)
				return controlPeerEvidence{UID: 2000, PID: 4242, ProcStart: "other-user-start"}, nil
			},
			AuthorizeHello: func(_ context.Context, _ controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
				authorizations.Add(1)
				return controlPrincipal{Role: hello.Role}, nil
			},
		})
		client, serverErr := startControlTestSession(t, server)
		writeControlTestFrame(t, client, map[string]any{
			"type": "hello", "version": 1, "request_id": "forged-owner", "role": "admin",
			"uid": 1000, "pid": 4242, "proc_start": "other-user-start",
		})
		assertNoSuccessfulControlHello(t, client)
		_ = client.Close()
		_ = waitControlTestServer(t, serverErr)
		if got := observations.Load(); got != 1 {
			t.Fatalf("kernel peer observations = %d, want 1", got)
		}
		if got := authorizations.Load(); got != 0 {
			t.Fatalf("role authorizations = %d, want 0 before same-user validation", got)
		}
	})

	t.Run("requested role remains subject to evidence authorization", func(t *testing.T) {
		var dispatches atomic.Int32
		server := newControlTestServer(controlServerConfig{
			Generation:     42,
			RuntimeVersion: "0.3.0",
			AuthorizeHello: func(_ context.Context, peer controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
				if hello.Role == controlRoleConnector {
					return controlPrincipal{}, &controlError{
						Code: "role_not_authorized", Message: "observed executable is not an authorized connector", Retryable: false,
					}
				}
				return controlPrincipal{Role: hello.Role, Peer: peer}, nil
			},
			Dispatch: func(context.Context, controlPrincipal, controlRequest) (controlDispatchResult, *controlError) {
				dispatches.Add(1)
				return controlDispatchResult{}, nil
			},
		})
		client, serverErr := startControlTestSession(t, server)
		writeControlTestFrame(t, client, map[string]any{
			"type": "hello", "version": 1, "request_id": "claimed-connector", "role": "connector",
			"product": "qwen", "attachment_id": "attachment-1", "capability": "claimed-capability",
		})
		assertNoSuccessfulControlHello(t, client)
		_ = client.Close()
		_ = waitControlTestServer(t, serverErr)
		if got := dispatches.Load(); got != 0 {
			t.Fatalf("dispatches = %d, want 0 after role authorization failure", got)
		}
	})
}

func TestControlRoleOperationInventory(t *testing.T) {
	allowed := map[controlRole][]string{
		controlRoleAdmin: {
			"runtime.status", "runtime.doctor", "migration.inspect", "remove.inspect",
		},
		controlRoleLauncher: {
			"attachment.prepare", "attachment.adopt", "attachment.refresh", "attachment.detach",
			"lane.start", "lane.resume", "lane.followup", "lane.status", "lane.list",
			"lane.interrupt", "lane.collect", "lane.archive",
		},
		controlRoleHook: {
			"attachment.adopt", "attachment.refresh", "attachment.detach",
		},
		controlRoleConnector: {
			"mcp.forward",
			"peer.identity", "peer.discover", "peer.send", "peer.broadcast", "peer.inbox", "peer.rename",
			"lane.start", "lane.resume", "lane.followup", "lane.status", "lane.list",
			"lane.interrupt", "lane.collect", "lane.archive",
		},
		controlRoleService: {
			"runtime.status", "migration.inspect", "remove.inspect",
		},
	}
	allOperations := []string{
		"runtime.status", "runtime.doctor",
		"attachment.prepare", "attachment.adopt", "attachment.refresh", "attachment.detach",
		"mcp.forward",
		"peer.identity", "peer.discover", "peer.send", "peer.broadcast", "peer.inbox", "peer.rename",
		"lane.start", "lane.resume", "lane.followup", "lane.status", "lane.list", "lane.interrupt", "lane.collect", "lane.archive",
		"migration.inspect", "remove.inspect",
	}

	for role, roleOperations := range allowed {
		allowedSet := make(map[string]bool, len(roleOperations))
		for _, operation := range roleOperations {
			allowedSet[operation] = true
		}
		for _, operation := range allOperations {
			if got, want := controlRoleAllowsOperation(role, operation), allowedSet[operation]; got != want {
				t.Errorf("controlRoleAllowsOperation(%q, %q) = %v, want %v", role, operation, got, want)
			}
		}
	}

	for _, operation := range []string{"daemon.restart", "install.apply", "purge.apply", "unknown"} {
		for role := range allowed {
			if controlRoleAllowsOperation(role, operation) {
				t.Errorf("role %q accepted unlisted online operation %q", role, operation)
			}
		}
	}
	for _, operation := range []string{"runtime.status", "runtime.doctor", "migration.inspect", "remove.inspect"} {
		if controlRoleAllowsOperation(controlRoleConnector, operation) {
			t.Errorf("model-facing connector accepted administrative operation %q", operation)
		}
	}
}

func TestControlRoleViolationIsRejectedBeforeDispatch(t *testing.T) {
	var dispatches atomic.Int32
	server := newControlTestServer(controlServerConfig{
		Generation:     42,
		RuntimeVersion: "0.3.0",
		AuthorizeHello: func(_ context.Context, peer controlPeerEvidence, hello controlHello) (controlPrincipal, *controlError) {
			return controlPrincipal{Role: hello.Role, AttachmentID: hello.AttachmentID, SessionID: "session-1", Peer: peer}, nil
		},
		Dispatch: func(_ context.Context, _ controlPrincipal, _ controlRequest) (controlDispatchResult, *controlError) {
			dispatches.Add(1)
			return controlDispatchResult{}, nil
		},
	})
	client, serverErr := startControlTestSession(t, server)
	helloControlTestSession(t, client, "connector")
	writeControlTestFrame(t, client, map[string]any{
		"type": "request", "version": 1, "request_id": "forbidden-admin",
		"operation": "runtime.doctor", "expected_generation": 42, "payload": map[string]any{},
	})
	response := readControlTestFrame(t, client)
	assertRejectedControlResponse(t, response, "forbidden-admin", "runtime.doctor", "operation_forbidden")
	if got := dispatches.Load(); got != 0 {
		t.Fatalf("dispatches = %d, want 0 for role-forbidden operation", got)
	}
	_ = client.Close()
	_ = waitControlTestServer(t, serverErr)
}

var controlTestPeerEvidence = controlPeerEvidence{
	UID: 1000, PID: 4242, ProcStart: "test-proc-start", StrongStart: "test-strong-start",
}

type testPersistedControlReplayStore struct {
	mu      sync.Mutex
	entries map[string][]byte
}

func newTestPersistedControlReplayStore() *testPersistedControlReplayStore {
	return &testPersistedControlReplayStore{entries: make(map[string][]byte)}
}

func (s *testPersistedControlReplayStore) lookup(_ context.Context, key controlReplayKey) (controlReplayEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, err := json.Marshal(key)
	if err != nil {
		return controlReplayEntry{}, false, err
	}
	body, ok := s.entries[string(encoded)]
	if !ok {
		return controlReplayEntry{}, false, nil
	}
	var entry controlReplayEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return controlReplayEntry{}, false, err
	}
	return entry, true, nil
}

func (s *testPersistedControlReplayStore) commit(_ context.Context, key controlReplayKey, entry controlReplayEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, err := json.Marshal(key)
	if err != nil {
		return err
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	s.entries[string(encoded)] = body
	return nil
}

func (s *testPersistedControlReplayStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func newControlTestServer(config controlServerConfig) *controlServer {
	if config.OwnerUID == 0 {
		config.OwnerUID = controlTestPeerEvidence.UID
	}
	if config.ObservePeer == nil {
		config.ObservePeer = func(net.Conn) (controlPeerEvidence, error) {
			return controlTestPeerEvidence, nil
		}
	}
	return newControlServer(config)
}

func roundTripControlTestRequest(
	t *testing.T,
	server *controlServer,
	attachmentID string,
	request map[string]any,
) map[string]any {
	t.Helper()
	client, serverErr := startControlTestSession(t, server)
	writeControlTestFrame(t, client, map[string]any{
		"type": "hello", "version": 1, "request_id": "hello-" + attachmentID, "role": "connector",
		"product": "qwen", "attachment_id": attachmentID, "session_id": "session-for-" + attachmentID, "capability": "test-capability",
	})
	if hello := readControlTestFrame(t, client); hello["type"] != "hello.result" {
		t.Fatalf("hello response = %#v", hello)
	}
	writeControlTestFrame(t, client, request)
	response := readControlTestFrame(t, client)
	_ = client.Close()
	_ = waitControlTestServer(t, serverErr)
	return response
}

func startControlTestSession(t *testing.T, server *controlServer) (net.Conn, <-chan error) {
	t.Helper()
	client, daemon := net.Pipe()
	deadline := time.Now().Add(controlTestTimeout)
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = daemon.Close() }()
		serverErr <- server.serveConn(context.Background(), daemon)
	}()
	return client, serverErr
}

func helloControlTestSession(t *testing.T, client net.Conn, role string) {
	t.Helper()
	hello := map[string]any{
		"type": "hello", "version": 1, "request_id": "hello-" + role, "role": role,
	}
	if role == "connector" {
		hello["product"] = "qwen"
		hello["attachment_id"] = "attachment-1"
		hello["session_id"] = "session-1"
		hello["capability"] = "test-capability"
	}
	writeControlTestFrame(t, client, hello)
	result := readControlTestFrame(t, client)
	if result["type"] != "hello.result" || result["request_id"] != hello["request_id"] {
		t.Fatalf("hello response = %#v", result)
	}
}

func writeControlTestFrame(t *testing.T, writer net.Conn, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal control frame: %v", err)
	}
	writeRawControlTestLine(t, writer, body)
}

func writeRawControlTestLine(t *testing.T, writer net.Conn, body []byte) {
	t.Helper()
	if _, err := writer.Write(append(body, '\n')); err != nil {
		t.Fatalf("write control frame: %v", err)
	}
}

func readControlTestFrame(t *testing.T, reader net.Conn) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(reader)
	var frame map[string]any
	if err := decoder.Decode(&frame); err != nil {
		t.Fatalf("decode control frame: %v", err)
	}
	return frame
}

func waitControlTestServer(t *testing.T, serverErr <-chan error) error {
	t.Helper()
	select {
	case err := <-serverErr:
		return err
	case <-time.After(controlTestTimeout):
		t.Fatal("timed out waiting for control server")
		return nil
	}
}

func assertRejectedControlResponse(t *testing.T, response map[string]any, requestID, operation, code string) {
	t.Helper()
	if response["type"] != "response" || response["request_id"] != requestID || response["operation"] != operation {
		t.Fatalf("uncorrelated rejected response = %#v", response)
	}
	if response["accepted"] != false { //nolint:revive // Dynamic JSON value requires explicit Boolean comparison.
		t.Fatalf("response accepted = %#v, want false", response["accepted"])
	}
	errorValue, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("response error = %#v", response["error"])
	}
	if errorValue["code"] != code || errorValue["retryable"] == nil || errorValue["message"] == "" {
		t.Fatalf("response error = %#v, want code %q plus retryable and message", errorValue, code)
	}
}

func assertNoSuccessfulControlHello(t *testing.T, reader net.Conn) {
	t.Helper()
	decoder := json.NewDecoder(reader)
	var frame map[string]any
	if err := decoder.Decode(&frame); err == nil && frame["type"] == "hello.result" {
		t.Fatalf("unauthorized hello returned success: %#v", frame)
	}
}
