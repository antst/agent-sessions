package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestControlPeerIdentityIsContextOnlyAndAbsentOffConnection(t *testing.T) {
	if peer, ok := ControlPeerIdentity(nil); ok || peer != (localtransport.PeerIdentity{}) {
		t.Fatalf("nil context identity = %#v, %t", peer, ok)
	}
	if peer, ok := ControlPeerIdentity(context.Background()); ok || peer != (localtransport.PeerIdentity{}) {
		t.Fatalf("background context identity = %#v, %t", peer, ok)
	}

	want := localtransport.PeerIdentity{PID: 4242, UID: 1000}
	wantProcess := procinfo.Identity{PID: want.PID, Start: "display-start", StrongStart: "strong-start"}
	ctx := withControlConnectionIdentity(context.Background(), want, wantProcess)
	if got, ok := ControlPeerIdentity(ctx); !ok || got != want {
		t.Fatalf("connection context identity = %#v, %t; want %#v, true", got, ok, want)
	}
	if got, ok := ControlProcessIdentity(ctx); !ok || got != wantProcess {
		t.Fatalf("connection process identity = %#v, %t; want %#v, true", got, ok, wantProcess)
	}
	incomplete := withControlConnectionIdentity(context.Background(), want, procinfo.Identity{PID: want.PID, Start: "display-start"})
	if process, ok := ControlProcessIdentity(incomplete); ok || process != (procinfo.Identity{}) {
		t.Fatalf("incomplete strong identity accepted: %#v, %t", process, ok)
	}
	mismatched := withControlConnectionIdentity(context.Background(), want, procinfo.Identity{PID: want.PID + 1, Start: "display-start", StrongStart: "strong-start"})
	if process, ok := ControlProcessIdentity(mismatched); ok || process != (procinfo.Identity{}) {
		t.Fatalf("PID-mismatched strong identity accepted: %#v, %t", process, ok)
	}

	requestWire, err := json.Marshal(ControlRequest{ID: "wire", Role: RoleAdmin, Operation: "status", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	responseWire, err := json.Marshal(ControlResponse{ID: "wire", Generation: 1, OK: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range [][]byte{requestWire, responseWire} {
		if strings.Contains(string(wire), `"pid"`) || strings.Contains(string(wire), `"uid"`) {
			t.Fatalf("control wire serialized peer identity: %s", wire)
		}
	}
}

func TestControlFramingIsBoundedCorrelatedAndStrict(t *testing.T) {
	request := ControlRequest{
		ID: "request-1", Role: RoleLauncher, Operation: "attachment.prepare", Generation: 7,
		IdempotencyKey: "prepare-1", Payload: json.RawMessage(`{"product":"codex"}`),
	}
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()
	done := make(chan error, 1)
	go func() { done <- writeControlFrame(client, request) }()
	var decoded ControlRequest
	if err := readControlFrame(server, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, request) {
		t.Fatalf("decoded frame = %#v, want %#v", decoded, request)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	overlongServer, overlongClient := net.Pipe()
	defer func() { _ = overlongServer.Close() }()
	defer func() { _ = overlongClient.Close() }()
	go func() {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], maxControlFrameBytes+1)
		_, _ = overlongClient.Write(header[:])
	}()
	if err := readControlFrame(overlongServer, &ControlRequest{}); err == nil || !strings.Contains(err.Error(), "frame") {
		t.Fatalf("oversize frame error = %v", err)
	}
}

func TestControlRolesGenerationBareInactivityAndNoLifecycleOverSocket(t *testing.T) {
	var handled atomic.Int64
	server := startControlTestServer(t, 7, func(_ context.Context, request ControlRequest) (json.RawMessage, error) {
		handled.Add(1)
		return json.RawMessage(`{"operation":"` + request.Operation + `"}`), nil
	})

	status := callControlTest(t, server.Endpoint(), ControlRequest{ID: "status", Role: RoleAdmin, Operation: "status", Generation: 7})
	if !status.OK || string(status.Payload) != `{"operation":"status"}` {
		t.Fatalf("admin status = %#v", status)
	}
	roster := callControlTest(t, server.Endpoint(), ControlRequest{ID: "roster", Role: RoleAdmin, Operation: "roster", Generation: 7})
	if !roster.OK || string(roster.Payload) != `{"operation":"roster"}` {
		t.Fatalf("admin roster = %#v", roster)
	}
	for _, request := range []ControlRequest{
		{ID: "wrong-role", Role: RoleAdmin, Operation: "attachment.prepare", Generation: 7, IdempotencyKey: "wrong"},
		{ID: "stale", Role: RoleLauncher, Operation: "attachment.prepare", Generation: 6, IdempotencyKey: "stale"},
		{ID: "lifecycle", Role: RoleAdmin, Operation: "service.restart", Generation: 7, IdempotencyKey: "lifecycle"},
	} {
		response := callControlTest(t, server.Endpoint(), request)
		if response.OK || response.Error == nil {
			t.Fatalf("rejected request succeeded: %#v => %#v", request, response)
		}
	}
	bare := callControlTest(t, server.Endpoint(), ControlRequest{
		ID: "bare", Role: RoleConnector, Operation: "connector.call", Generation: 7,
		IdempotencyKey: "bare-call", Payload: json.RawMessage(`{"tool":"list_peers"}`),
	})
	if bare.OK || bare.Error == nil || bare.Error.Code != ErrorInactive || bare.Error.Message != CanonicalInactiveMessage {
		t.Fatalf("bare connector response = %#v", bare)
	}
	if handled.Load() != 2 {
		t.Fatalf("handler calls = %d, want admin status and roster", handled.Load())
	}
}

func TestControlMutationIdempotencyIsExactAndConflictSafe(t *testing.T) {
	var handled atomic.Int64
	server := startControlTestServer(t, 11, func(_ context.Context, _ ControlRequest) (json.RawMessage, error) {
		count := handled.Add(1)
		return json.Marshal(map[string]int64{"count": count})
	})
	request := ControlRequest{
		ID: "prepare", Role: RoleLauncher, Operation: "attachment.prepare", Generation: 11,
		IdempotencyKey: "same", Payload: json.RawMessage(`{"product":"grok"}`),
	}
	first := callControlTest(t, server.Endpoint(), request)
	second := callControlTest(t, server.Endpoint(), request)
	if !first.OK || !second.OK || string(first.Payload) != string(second.Payload) || handled.Load() != 1 {
		t.Fatalf("idempotent replay = first %#v second %#v calls=%d", first, second, handled.Load())
	}
	request.ID = "conflict"
	request.Payload = json.RawMessage(`{"product":"qwen"}`)
	conflict := callControlTest(t, server.Endpoint(), request)
	if conflict.OK || conflict.Error == nil || conflict.Error.Code != ErrorIdempotencyConflict || handled.Load() != 1 {
		t.Fatalf("conflicting replay = %#v calls=%d", conflict, handled.Load())
	}
	missing := callControlTest(t, server.Endpoint(), ControlRequest{ID: "missing-key", Role: RoleHook, Operation: "hook.event", Generation: 11})
	if missing.OK || missing.Error == nil || missing.Error.Code != ErrorInvalidRequest {
		t.Fatalf("mutation without idempotency key = %#v", missing)
	}
}

func TestControlMutationCacheIsPartitionedByStrongProcessIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		role       ControlRole
		operation  string
		attachment string
	}{
		{name: "hook", role: RoleHook, operation: "hook.event"},
		{name: "connector", role: RoleConnector, operation: "connector.call", attachment: "attachment-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var handled int
			policy := &controlPolicy{
				generation: 17,
				handler: func(_ context.Context, _ ControlRequest) (json.RawMessage, error) {
					handled++
					return json.Marshal(map[string]int{"call": handled})
				},
				cache:    map[controlMutationCacheKey]cachedControlResponse{},
				inFlight: map[controlMutationCacheKey]*inFlightControlResponse{},
			}
			request := ControlRequest{
				ID: "first", Role: test.role, Operation: test.operation, Generation: 17,
				IdempotencyKey: "shared", AttachmentID: test.attachment, Payload: json.RawMessage(`{}`),
			}
			peer := localtransport.PeerIdentity{PID: 101, UID: 1000}
			firstProcess := procinfo.Identity{PID: peer.PID, Start: "same-display-start", StrongStart: "strong-start-a"}
			firstConnection := withControlConnectionIdentity(context.Background(), peer, firstProcess)
			retryConnection := withControlConnectionIdentity(context.Background(), peer, firstProcess)
			reusedPIDConnection := withControlConnectionIdentity(context.Background(), peer, procinfo.Identity{
				PID: peer.PID, Start: firstProcess.Start, StrongStart: "strong-start-b",
			})
			otherPeer := localtransport.PeerIdentity{PID: 202, UID: peer.UID}
			otherPeerConnection := withControlConnectionIdentity(context.Background(), otherPeer, procinfo.Identity{
				PID: otherPeer.PID, Start: "other-display-start", StrongStart: "other-strong-start",
			})

			first := policy.handle(firstConnection, request)
			request.ID = "exact-retry"
			exactRetry := policy.handle(retryConnection, request)
			request.ID = "reused-pid"
			reusedPID := policy.handle(reusedPIDConnection, request)
			request.ID = "other-peer"
			other := policy.handle(otherPeerConnection, request)
			if !first.OK || !exactRetry.OK || !reusedPID.OK || !other.OK {
				t.Fatalf("authority-partitioned responses = first %#v retry %#v reused %#v other %#v", first, exactRetry, reusedPID, other)
			}
			if string(first.Payload) != string(exactRetry.Payload) {
				t.Fatalf("exact process identity did not share cache: first=%s retry=%s", first.Payload, exactRetry.Payload)
			}
			if string(first.Payload) == string(reusedPID.Payload) {
				t.Fatalf("PID reuse shared cached response: first=%s reused=%s", first.Payload, reusedPID.Payload)
			}
			if string(reusedPID.Payload) == string(other.Payload) || handled != 3 {
				t.Fatalf("other peer response=%s reused=%s calls=%d", other.Payload, reusedPID.Payload, handled)
			}
		})
	}
}

func TestControlMutationFailsClosedWithoutStrongProcessIdentity(t *testing.T) {
	var handled atomic.Int64
	policy := &controlPolicy{
		generation: 17,
		handler: func(_ context.Context, _ ControlRequest) (json.RawMessage, error) {
			handled.Add(1)
			return json.RawMessage(`{}`), nil
		},
		cache:    map[controlMutationCacheKey]cachedControlResponse{},
		inFlight: map[controlMutationCacheKey]*inFlightControlResponse{},
	}
	peer := localtransport.PeerIdentity{PID: 101, UID: 1000}
	incomplete := withControlConnectionIdentity(context.Background(), peer, procinfo.Identity{
		PID: peer.PID, Start: "display-start",
	})
	response := policy.handle(incomplete, ControlRequest{
		ID: "missing-strong-start", Role: RoleHook, Operation: "hook.event", Generation: 17,
		IdempotencyKey: "missing-strong-start", Payload: json.RawMessage(`{}`),
	})
	if response.OK || response.Error == nil || response.Error.Code != ErrorHandler || handled.Load() != 0 {
		t.Fatalf("mutation without strong process identity = %#v, handler calls=%d", response, handled.Load())
	}
}

func TestControlMutationInFlightCoalescesExactStrongProcessIdentity(t *testing.T) {
	var handled atomic.Int64
	policy := &controlPolicy{
		generation: 18,
		handler: func(_ context.Context, _ ControlRequest) (json.RawMessage, error) {
			handled.Add(1)
			return json.RawMessage(`{"unexpected":true}`), nil
		},
		cache:    map[controlMutationCacheKey]cachedControlResponse{},
		inFlight: map[controlMutationCacheKey]*inFlightControlResponse{},
	}
	request := ControlRequest{
		ID: "in-flight-retry", Role: RoleHook, Operation: "hook.event", Generation: 18,
		IdempotencyKey: "in-flight", Payload: json.RawMessage(`{}`),
	}
	peer := localtransport.PeerIdentity{PID: 303, UID: 1000}
	process := procinfo.Identity{PID: peer.PID, Start: "display-start", StrongStart: "strong-start"}
	firstConnection := withControlConnectionIdentity(context.Background(), peer, process)
	retryConnection := withControlConnectionIdentity(context.Background(), peer, process)
	digest, err := controlRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	key := controlMutationAuthorityKey(firstConnection, request)
	if retryKey := controlMutationAuthorityKey(retryConnection, request); retryKey != key {
		t.Fatalf("exact process identity keys differ: first=%#v retry=%#v", key, retryKey)
	}
	active := &inFlightControlResponse{
		digest: digest,
		done:   make(chan struct{}),
		response: ControlResponse{
			OK: true, Payload: json.RawMessage(`{"coalesced":true}`),
		},
	}
	policy.inFlight[key] = active
	result := make(chan ControlResponse, 1)
	go func() { result <- policy.handle(retryConnection, request) }()
	close(active.done)
	select {
	case response := <-result:
		if !response.OK || string(response.Payload) != `{"coalesced":true}` || handled.Load() != 0 {
			t.Fatalf("in-flight coalescing response=%#v handler calls=%d", response, handled.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("exact process identity did not coalesce with in-flight mutation")
	}
}

func TestControlMutationsWithDifferentKeysDoNotSerializeNestedWork(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := startControlTestServer(t, 13, func(_ context.Context, request ControlRequest) (json.RawMessage, error) {
		if request.IdempotencyKey == "outer" {
			close(entered)
			<-release
		}
		return json.RawMessage(`{"ok":true}`), nil
	})
	outerDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := CallControl(ctx, server.Endpoint(), ControlRequest{
			ID: "outer", Role: RoleLauncher, Operation: "lane.command", Generation: 13,
			IdempotencyKey: "outer", Payload: json.RawMessage(`{}`),
		})
		outerDone <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inner, err := CallControl(ctx, server.Endpoint(), ControlRequest{
		ID: "inner", Role: RoleConnector, Operation: "connector.call", Generation: 13,
		IdempotencyKey: "inner", AttachmentID: "lane", Payload: json.RawMessage(`{}`),
	})
	if err != nil || !inner.OK {
		t.Fatalf("nested mutation was serialized behind outer turn: response=%#v err=%v", inner, err)
	}
	close(release)
	if err := <-outerDone; err != nil {
		t.Fatal(err)
	}
}

func startControlTestServer(t *testing.T, generation uint64, handler ControlHandler) *ControlServer {
	t.Helper()
	server, err := StartControlServer(context.Background(), shortDaemonTestRoot(t), generation, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close control server: %v", err)
		}
	})
	return server
}

func callControlTest(t *testing.T, endpoint string, request ControlRequest) ControlResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := CallControl(ctx, endpoint, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != request.ID {
		t.Fatalf("response correlation = %q, want %q", response.ID, request.ID)
	}
	return response
}
