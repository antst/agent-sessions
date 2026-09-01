package component

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestBrokerBootstrapReplayHeartbeatAndDistinctNativeSession(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("CaptureIdentity: %v", err)
	}
	authorizer := &testAuthorizer{secret: "one-time-value", process: identity}
	var mu sync.Mutex
	var handled []Frame
	broker, err := NewBroker(Config{
		Generation: 7, Authorizer: authorizer, HeartbeatInterval: 100 * time.Millisecond,
		Handler: HandlerFunc(func(_ context.Context, binding BindingView, frame Frame) error {
			if binding.AttachmentID != "attachment-lifecycle" {
				t.Fatalf("handler attachment = %q", binding.AttachmentID)
			}
			mu.Lock()
			handled = append(handled, frame)
			mu.Unlock()
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	connection := dialFrameConn(t, path)
	defer connection.Close()

	writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{
		ProductID: "pi", AttachmentID: "attachment-lifecycle", BootstrapCapabilityID: "capability-1",
		BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart,
		ComponentVersion: "1.0.0",
	})
	readyFrame := readTestFrame(t, connection)
	if readyFrame.Type != TypeReady {
		t.Fatalf("first response = %s, want ready", readyFrame.Type)
	}
	var ready Ready
	if err := readyFrame.PayloadInto(&ready); err != nil {
		t.Fatal(err)
	}
	if ready.AttachmentID != "attachment-lifecycle" || ready.DaemonGeneration != 7 || ready.BindingID == "" {
		t.Fatalf("ready = %#v", ready)
	}

	announce := SessionAnnounce{BindingID: ready.BindingID, NativeSessionID: "native-session-separate", Cwd: "/work", NativeName: "native", ProductEventSeq: 1}
	writeTestFrame(t, connection, TypeSessionAnnounce, "announce", 2, announce)
	writeTestFrame(t, connection, TypeSessionAnnounce, "announce", 2, announce)
	writeTestFrame(t, connection, TypeHeartbeat, "heartbeat", 3, Heartbeat{BindingID: ready.BindingID, LastReceivedSeq: 1})
	ack := readTestFrame(t, connection)
	if ack.Type != TypeHeartbeatAck {
		t.Fatalf("heartbeat response = %s", ack.Type)
	}
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == 1
	})
	if err := broker.Send(ready.BindingID, TypeDeliveryPresent, "delivery", DeliveryPresent{DeliveryID: "delivery", Mode: "wake", Body: json.RawMessage(`{"text":"hello"}`)}); err != nil {
		t.Fatalf("Send delivery.present: %v", err)
	}
	if present := readTestFrame(t, connection); present.Type != TypeDeliveryPresent {
		t.Fatalf("daemon frame = %s, want delivery.present", present.Type)
	}
	accept := DeliveryAccept{DeliveryID: "delivery", NativeSessionID: "native-session-separate", NativeMessageID: "native-message", AcceptedAt: 1}
	writeTestFrame(t, connection, TypeDeliveryAccept, "accept-one", 4, accept)
	writeTestFrame(t, connection, TypeDeliveryAccept, "accept-replay", 5, accept)
	writeTestFrame(t, connection, TypeHeartbeat, "heartbeat-after-replay", 6, Heartbeat{BindingID: ready.BindingID, LastReceivedSeq: 3})
	if replayAck := readTestFrame(t, connection); replayAck.Type != TypeHeartbeatAck {
		t.Fatalf("post-replay frame = %s, want heartbeat.ack", replayAck.Type)
	}
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if handled[0].Type != TypeSessionAnnounce {
		t.Fatalf("handled frame = %s", handled[0].Type)
	}
	if handled[1].Type != TypeDeliveryAccept {
		t.Fatalf("handled replayable operation = %s", handled[1].Type)
	}
}

func TestBrokerRejectsWrongProcessAndPIDReuseEvidence(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		claimStart  string
		claimStrong string
		authProcess procinfo.Identity
	}{
		{name: "wrong-process-claim", claimStart: "wrong", claimStrong: identity.StrongStart, authProcess: identity},
		{name: "pid-reuse-after-authorization", claimStart: identity.Start, claimStrong: identity.StrongStart, authProcess: procinfo.Identity{PID: identity.PID, Start: identity.Start, StrongStart: identity.StrongStart + "-reused"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			authorizer := &testAuthorizer{secret: "raw-bootstrap", process: test.authProcess}
			broker, err := NewBroker(Config{Generation: 8, Authorizer: authorizer, HeartbeatInterval: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			path, stop := serveTestBroker(t, broker)
			defer stop()
			connection := dialFrameConn(t, path)
			defer connection.Close()
			writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{
				ProductID: "omp", AttachmentID: "attachment", BootstrapCapabilityID: "capability",
				BootstrapValue: authorizer.secret, ProcessStart: test.claimStart, StrongStart: test.claimStrong,
				ComponentVersion: "1",
			})
			reject := readTestFrame(t, connection)
			if reject.Type != TypeReject {
				t.Fatalf("response = %s, want reject", reject.Type)
			}
			var payload Reject
			if err := reject.PayloadInto(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Category != CategoryStaleProcess {
				t.Fatalf("reject category = %q", payload.Category)
			}
			if contains(payload.Detail, authorizer.secret) {
				t.Fatalf("reject leaked bootstrap value: %q", payload.Detail)
			}
		})
	}
}

func TestBrokerConsumesBootstrapCapabilityOnceAndRedactsRefusal(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "one-time-raw-value", process: identity}
	broker, err := NewBroker(Config{Generation: 3, Authorizer: authorizer, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	claim := BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: "1"}
	first := dialFrameConn(t, path)
	writeTestFrame(t, first, TypeBootstrap, "first", 1, claim)
	if response := readTestFrame(t, first); response.Type != TypeReady {
		t.Fatalf("first bootstrap response = %s", response.Type)
	}
	first.Close()

	second := dialFrameConn(t, path)
	defer second.Close()
	writeTestFrame(t, second, TypeBootstrap, "second", 1, claim)
	response := readTestFrame(t, second)
	if response.Type != TypeReject {
		t.Fatalf("reused bootstrap response = %s", response.Type)
	}
	var rejection Reject
	if err := response.PayloadInto(&rejection); err != nil {
		t.Fatal(err)
	}
	if rejection.Category != CategoryUnauthorized || contains(rejection.Detail, authorizer.secret) {
		t.Fatalf("reused bootstrap rejection = %#v", rejection)
	}
}

func TestBrokerReconnectReattestsSameProcessAcrossGeneration(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	first, _ := NewBroker(Config{Generation: 1, Authorizer: authorizer, HeartbeatInterval: time.Second})
	path, stopFirst := serveTestBroker(t, first)
	connection := dialFrameConn(t, path)
	writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "opencode", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: "1"})
	readyOneFrame := readTestFrame(t, connection)
	var readyOne Ready
	_ = readyOneFrame.PayloadInto(&readyOne)
	connection.Close()
	stopFirst()

	second, _ := NewBroker(Config{Generation: 2, Authorizer: authorizer, HeartbeatInterval: time.Second})
	path, stopSecond := serveTestBrokerAt(t, second, path)
	defer stopSecond()
	reconnected := dialFrameConn(t, path)
	defer reconnected.Close()
	writeTestFrame(t, reconnected, TypeReconnect, "reconnect", 1, ReconnectClaim{
		AttachmentID: "attachment", PriorBindingID: readyOne.BindingID, PriorGeneration: 1,
		ProcessStart: identity.Start, StrongStart: identity.StrongStart, LastReceivedSeq: readyOneFrame.Seq,
	})
	readyTwoFrame := readTestFrame(t, reconnected)
	var readyTwo Ready
	if readyTwoFrame.Type != TypeReady || readyTwoFrame.PayloadInto(&readyTwo) != nil {
		t.Fatalf("reconnect response = %#v", readyTwoFrame)
	}
	if readyTwo.DaemonGeneration != 2 || readyTwo.BindingID == readyOne.BindingID || authorizer.reconnects != 1 {
		t.Fatalf("reconnect ready = %#v, reconnects=%d", readyTwo, authorizer.reconnects)
	}
}

func TestBrokerHeartbeatTimeoutClosesBinding(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	broker, _ := NewBroker(Config{Generation: 1, Authorizer: authorizer, HeartbeatInterval: 20 * time.Millisecond, HeartbeatGrace: 2})
	path, stop := serveTestBroker(t, broker)
	defer stop()
	connection := dialFrameConn(t, path)
	defer connection.Close()
	writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: "1"})
	_ = readTestFrame(t, connection)
	time.Sleep(80 * time.Millisecond)
	if _, err := connection.ReadFrame(); err == nil {
		t.Fatal("connection survived missed heartbeat deadline")
	}
	eventually(t, func() bool { return len(broker.Bindings()) == 0 })
}

type testAuthorizer struct {
	mu         sync.Mutex
	secret     string
	process    procinfo.Identity
	used       bool
	reconnects int
}

func (a *testAuthorizer) Bootstrap(_ context.Context, claim BootstrapClaim, evidence PeerEvidence) (Authorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used || claim.BootstrapValue != a.secret {
		return Authorization{}, errors.New("bootstrap_value=" + claim.BootstrapValue + " was refused")
	}
	a.used = true
	return Authorization{AttachmentID: claim.AttachmentID, ProductID: claim.ProductID, ProcessIdentity: a.process, BootstrapRevision: 1}, nil
}

func (a *testAuthorizer) Reconnect(_ context.Context, claim ReconnectClaim, evidence PeerEvidence) (Authorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconnects++
	return Authorization{AttachmentID: claim.AttachmentID, ProductID: "opencode", ProcessIdentity: a.process, BootstrapRevision: 1}, nil
}

func serveTestBroker(t *testing.T, broker *Broker) (string, func()) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return serveTestBrokerAt(t, broker, filepath.Join(root, ComponentSocketName))
}

func serveTestBrokerAt(t *testing.T, broker *Broker, path string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Serve(ctx, path) }()
	eventually(t, func() bool {
		connection, err := net.DialTimeout("unix", path, 10*time.Millisecond)
		if err != nil {
			return false
		}
		connection.Close()
		return true
	})
	var once sync.Once
	return path, func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve: %v", err)
				}
			case <-time.After(time.Second):
				t.Error("Serve did not stop")
			}
		})
	}
}

func dialFrameConn(t *testing.T, path string) *localtransport.Conn {
	t.Helper()
	connection, err := localtransport.Dial(path, localtransport.DefaultLimits())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return connection
}

func writeTestFrame(t *testing.T, connection *localtransport.Conn, frameType FrameType, id string, seq uint64, payload any) {
	t.Helper()
	frame, err := NewFrame(frameType, id, seq, payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteFrame(body); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
}

func readTestFrame(t *testing.T, connection *localtransport.Conn) Frame {
	t.Helper()
	body, err := connection.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	frame, err := DecodeFrame(body)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	return frame
}

func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func contains(value, fragment string) bool {
	return fragment != "" && len(value) >= len(fragment) && (value == fragment || len(value) > len(fragment) && (value[:len(fragment)] == fragment || contains(value[1:], fragment)))
}
