package component

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
)

var noopComponentHandler = HandlerFunc(func(context.Context, BindingView, Frame) error { return nil })

func TestBrokerRequiresDurableHandlerAndBoundsDisconnectedDeliveryArchives(t *testing.T) {
	authorizer := &testAuthorizer{}
	if _, err := NewBroker(Config{Generation: 1, Authorizer: authorizer, HeartbeatInterval: time.Second}); err == nil {
		t.Fatal("broker accepted a nil durable handler")
	}
	broker, err := NewBroker(Config{
		Generation: 1, Authorizer: authorizer, Handler: noopComponentHandler,
		HeartbeatInterval: time.Second, MaxDeliveryArchives: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker.mu.Lock()
	for index := 0; index < 1_000; index++ {
		attachmentID := fmt.Sprintf("attachment-%d", index)
		broker.archiveAttachmentDelivery(attachmentID, attachmentDeliveryState{
			priorBindingID: fmt.Sprintf("binding-%d", index), tracker: newDeliveryTracker(1, 1),
		})
		if len(broker.attachmentDelivery) > 3 || len(broker.attachmentDeliveryOrder) > 3 {
			broker.mu.Unlock()
			t.Fatalf("delivery archive bound widened at churn iteration %d", index)
		}
	}
	broker.archiveAttachmentDelivery("attachment-999", attachmentDeliveryState{
		priorBindingID: "binding-999", tracker: newDeliveryTracker(1, 1),
	})
	if len(broker.attachmentDelivery) != 3 || len(broker.attachmentDeliveryOrder) != 3 {
		broker.mu.Unlock()
		t.Fatalf("bounded delivery archives = %d / %d", len(broker.attachmentDelivery), len(broker.attachmentDeliveryOrder))
	}
	seen := make(map[string]struct{})
	for _, attachmentID := range broker.attachmentDeliveryOrder {
		if _, duplicate := seen[attachmentID]; duplicate {
			broker.mu.Unlock()
			t.Fatalf("delivery archive order leaked duplicate key %q", attachmentID)
		}
		seen[attachmentID] = struct{}{}
		if _, exists := broker.attachmentDelivery[attachmentID]; !exists {
			broker.mu.Unlock()
			t.Fatalf("delivery archive order retained missing key %q", attachmentID)
		}
	}
	broker.mu.Unlock()
}

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
		ComponentVersion: ContractRevision,
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

	announce := SessionAnnounce{BindingID: ready.BindingID, NativeSessionID: "native-session-separate", Cwd: "/work", NativeName: "", ProductEventSeq: 1}
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
		return len(handled) == 2
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
		return len(handled) == 4
	})
	mu.Lock()
	defer mu.Unlock()
	if handled[0].Type != TypeSessionAnnounce {
		t.Fatalf("handled frame = %s", handled[0].Type)
	}
	var handledAnnounce SessionAnnounce
	if err := handled[0].PayloadInto(&handledAnnounce); err != nil || handledAnnounce.NativeName != "" {
		t.Fatalf("authenticated empty-title announce = %#v / %v", handledAnnounce, err)
	}
	if handled[1].Type != TypeHeartbeat || handled[2].Type != TypeDeliveryAccept || handled[3].Type != TypeHeartbeat {
		t.Fatalf("durably handled operation order = %s, %s, %s", handled[1].Type, handled[2].Type, handled[3].Type)
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
			broker, err := NewBroker(Config{Generation: 8, Authorizer: authorizer, Handler: noopComponentHandler, HeartbeatInterval: time.Second})
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
				ComponentVersion: ContractRevision,
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
	broker, err := NewBroker(Config{Generation: 3, Authorizer: authorizer, Handler: noopComponentHandler, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	claim := BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: ContractRevision}
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
	first, _ := NewBroker(Config{Generation: 1, Authorizer: authorizer, Handler: noopComponentHandler, HeartbeatInterval: time.Second})
	path, stopFirst := serveTestBroker(t, first)
	connection := dialFrameConn(t, path)
	writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "opencode", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: ContractRevision})
	readyOneFrame := readTestFrame(t, connection)
	var readyOne Ready
	_ = readyOneFrame.PayloadInto(&readyOne)
	connection.Close()
	stopFirst()

	second, _ := NewBroker(Config{Generation: 2, Authorizer: authorizer, Handler: noopComponentHandler, HeartbeatInterval: time.Second})
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

func TestBrokerIdempotentHandshakeReadyLossRestartAndAdoptionFence(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &durableHandshakeAuthorizer{
		secret: "one-time-bootstrap", process: identity, attachmentID: "attachment-durable", productID: "pi",
		capabilityID: "capability-durable", revision: 7,
	}
	handler := HandlerFunc(func(_ context.Context, binding BindingView, frame Frame) error {
		if frame.Type == TypeSessionAnnounce {
			return authorizer.adopt(binding.BindingID)
		}
		return nil
	})
	newBroker := func(generation uint64) *Broker {
		broker, brokerErr := NewBroker(Config{
			Generation: generation, Authorizer: authorizer, Handler: handler, HeartbeatInterval: time.Second,
		})
		if brokerErr != nil {
			t.Fatal(brokerErr)
		}
		return broker
	}
	claim := BootstrapClaim{
		ProductID: authorizer.productID, AttachmentID: authorizer.attachmentID,
		BootstrapCapabilityID: authorizer.capabilityID, BootstrapValue: authorizer.secret,
		ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: ContractRevision,
	}

	firstBroker := newBroker(1)
	path, stopFirst := serveTestBroker(t, firstBroker)
	first := dialFrameConn(t, path)
	writeTestFrame(t, first, TypeBootstrap, "bootstrap-first", 1, claim)
	firstReadyFrame := readTestFrame(t, first)
	var firstReady Ready
	if firstReadyFrame.Type != TypeReady || firstReadyFrame.PayloadInto(&firstReady) != nil {
		t.Fatalf("first bootstrap ready = %#v", firstReadyFrame)
	}
	first.Close()
	eventually(t, func() bool { return len(firstBroker.Bindings()) == 0 })

	foreignEvidence := PeerEvidence{Peer: localtransport.PeerIdentity{PID: identity.PID, UID: os.Geteuid()}, Process: identity}
	foreignEvidence.Process.StrongStart += "-foreign"
	if _, err := authorizer.Bootstrap(context.Background(), claim, foreignEvidence); err == nil {
		t.Fatal("foreign process reused the exact bootstrap capability")
	}

	replay := dialFrameConn(t, path)
	writeTestFrame(t, replay, TypeBootstrap, "bootstrap-ready-loss", 1, claim)
	replayReadyFrame := readTestFrame(t, replay)
	var replayReady Ready
	if replayReadyFrame.PayloadInto(&replayReady) != nil || replayReady.BindingID != firstReady.BindingID {
		t.Fatalf("bootstrap Ready-loss binding = %#v, want %q", replayReady, firstReady.BindingID)
	}
	replay.Close()
	eventually(t, func() bool { return len(firstBroker.Bindings()) == 0 })
	stopFirst()

	secondBroker := newBroker(2)
	_, stopSecond := serveTestBrokerAt(t, secondBroker, path)
	defer stopSecond()
	restarted := dialFrameConn(t, path)
	writeTestFrame(t, restarted, TypeBootstrap, "bootstrap-daemon-restart", 1, claim)
	restartedReadyFrame := readTestFrame(t, restarted)
	var restartedReady Ready
	if restartedReadyFrame.PayloadInto(&restartedReady) != nil || restartedReady.BindingID != firstReady.BindingID {
		t.Fatalf("daemon-restart bootstrap binding = %#v, want %q", restartedReady, firstReady.BindingID)
	}
	writeTestFrame(t, restarted, TypeSessionAnnounce, "adopt-bootstrap", 2, SessionAnnounce{
		BindingID: restartedReady.BindingID, NativeSessionID: "native-distinct", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	eventually(t, func() bool { return authorizer.isCurrent(restartedReady.BindingID) })
	restarted.Close()
	eventually(t, func() bool { return len(secondBroker.Bindings()) == 0 })

	reconnectClaim := ReconnectClaim{
		AttachmentID: authorizer.attachmentID, PriorBindingID: restartedReady.BindingID, PriorGeneration: 2,
		ProcessStart: identity.Start, StrongStart: identity.StrongStart, LastReceivedSeq: restartedReadyFrame.Seq,
	}
	firstReconnect := dialFrameConn(t, path)
	writeTestFrame(t, firstReconnect, TypeReconnect, "reconnect-first", 1, reconnectClaim)
	firstReconnectReadyFrame := readTestFrame(t, firstReconnect)
	var firstReconnectReady Ready
	if firstReconnectReadyFrame.PayloadInto(&firstReconnectReady) != nil || firstReconnectReady.BindingID == restartedReady.BindingID {
		t.Fatalf("first reconnect Ready = %#v", firstReconnectReady)
	}
	firstReconnect.Close()
	eventually(t, func() bool { return len(secondBroker.Bindings()) == 0 })

	reconnectReplay := dialFrameConn(t, path)
	writeTestFrame(t, reconnectReplay, TypeReconnect, "reconnect-ready-loss", 1, reconnectClaim)
	reconnectReplayReadyFrame := readTestFrame(t, reconnectReplay)
	var reconnectReplayReady Ready
	if reconnectReplayReadyFrame.PayloadInto(&reconnectReplayReady) != nil || reconnectReplayReady.BindingID != firstReconnectReady.BindingID {
		t.Fatalf("reconnect Ready-loss binding = %#v, want %q", reconnectReplayReady, firstReconnectReady.BindingID)
	}
	if authorizer.successorCount() != 1 {
		t.Fatalf("unacknowledged reconnect successors = %d, want one", authorizer.successorCount())
	}
	writeTestFrame(t, reconnectReplay, TypeSessionAnnounce, "adopt-reconnect", 2, SessionAnnounce{
		BindingID: reconnectReplayReady.BindingID, NativeSessionID: "native-distinct", Cwd: "/work", NativeName: "native", ProductEventSeq: 2,
	})
	eventually(t, func() bool { return authorizer.isCurrent(reconnectReplayReady.BindingID) })

	stale := dialFrameConn(t, path)
	defer stale.Close()
	writeTestFrame(t, stale, TypeReconnect, "stale-predecessor", 1, reconnectClaim)
	staleReject := readTestFrame(t, stale)
	if staleReject.Type != TypeReject {
		t.Fatalf("post-adoption predecessor response = %s, want reject", staleReject.Type)
	}
	reconnectReplay.Close()
}

func TestBrokerReconnectsAfterSameGenerationSocketClose(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	handled := make(chan Frame, 1)
	broker, err := NewBroker(Config{
		Generation: 4, Authorizer: authorizer, HeartbeatInterval: time.Second,
		Handler: HandlerFunc(func(_ context.Context, _ BindingView, frame Frame) error {
			handled <- frame
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	first := dialFrameConn(t, path)
	writeTestFrame(t, first, TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: ContractRevision})
	firstReadyFrame := readTestFrame(t, first)
	var firstReady Ready
	if err := firstReadyFrame.PayloadInto(&firstReady); err != nil {
		t.Fatal(err)
	}
	if err := broker.Send(firstReady.BindingID, TypeDeliveryPresent, "delivery-boundary", DeliveryPresent{
		DeliveryID: "delivery-boundary", Mode: "wake", Body: json.RawMessage(`{"text":"hello"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if present := readTestFrame(t, first); present.Type != TypeDeliveryPresent {
		t.Fatalf("delivery frame before reconnect = %s", present.Type)
	}
	if err := broker.Send(firstReady.BindingID, TypeDeliveryPresent, "delivery-committed", DeliveryPresent{
		DeliveryID: "delivery-committed", Mode: "wake", Body: json.RawMessage(`{"text":"committed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	committedPresent := readTestFrame(t, first)
	if committedPresent.Type != TypeDeliveryPresent {
		t.Fatalf("committed delivery frame before reconnect = %s", committedPresent.Type)
	}
	committedAccept := DeliveryAccept{
		DeliveryID: "delivery-committed", NativeSessionID: "native-distinct", NativeMessageID: "committed-message", AcceptedAt: 1,
	}
	writeTestFrame(t, first, TypeDeliveryAccept, "delivery-committed", 2, committedAccept)
	select {
	case frame := <-handled:
		if frame.Type != TypeDeliveryAccept || frame.ID != "delivery-committed" {
			t.Fatalf("committed delivery handler frame = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery acceptance did not reach the handler before disconnect")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return len(broker.Bindings()) == 0 })

	second := dialFrameConn(t, path)
	defer second.Close()
	writeTestFrame(t, second, TypeReconnect, "reconnect", 1, ReconnectClaim{
		AttachmentID: "attachment", PriorBindingID: firstReady.BindingID, PriorGeneration: 4,
		ProcessStart: identity.Start, StrongStart: identity.StrongStart, LastReceivedSeq: committedPresent.Seq,
	})
	secondReadyFrame := readTestFrame(t, second)
	var secondReady Ready
	if err := secondReadyFrame.PayloadInto(&secondReady); err != nil {
		t.Fatal(err)
	}
	if secondReadyFrame.Type != TypeReady || secondReady.DaemonGeneration != 4 || secondReady.BindingID == firstReady.BindingID {
		t.Fatalf("same-generation ready = %#v / %#v", secondReadyFrame, secondReady)
	}
	if authorizer.reconnects != 1 {
		t.Fatalf("same-generation authorizer reconnect calls = %d", authorizer.reconnects)
	}
	writeTestFrame(t, second, TypeDeliveryAccept, "delivery-boundary", 2, DeliveryAccept{
		DeliveryID: "delivery-boundary", NativeSessionID: "native-distinct", NativeMessageID: "message", AcceptedAt: 1,
	})
	select {
	case frame := <-handled:
		if frame.Type != TypeDeliveryAccept || frame.ID != "delivery-boundary" {
			t.Fatalf("replayed delivery handler frame = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("replayed delivery acceptance was not admitted on the fresh binding")
	}
	writeTestFrame(t, second, TypeDeliveryAccept, "delivery-committed", 3, committedAccept)
	writeTestFrame(t, second, TypeHeartbeat, "heartbeat", 4, Heartbeat{BindingID: secondReady.BindingID, LastReceivedSeq: secondReadyFrame.Seq})
	if ack := readTestFrame(t, second); ack.Type != TypeHeartbeatAck {
		t.Fatalf("frame after replayed delivery acceptance = %s, want heartbeat.ack", ack.Type)
	}
	select {
	case frame := <-handled:
		if frame.Type != TypeHeartbeat {
			t.Fatalf("already committed delivery replay reached handler: %#v", frame)
		}
	default:
		t.Fatal("heartbeat ack was sent before durable handler admission")
	}
}

func TestBrokerSameAttachmentReconnectFencesOldStreamAndRejectsStaleDualStream(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	broker, err := NewBroker(Config{Generation: 6, Authorizer: authorizer, Handler: noopComponentHandler, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	oldStream := dialFrameConn(t, path)
	defer oldStream.Close()
	writeTestFrame(t, oldStream, TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "omp", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: ContractRevision})
	oldReadyFrame := readTestFrame(t, oldStream)
	var oldReady Ready
	if err := oldReadyFrame.PayloadInto(&oldReady); err != nil {
		t.Fatal(err)
	}

	winningStream := dialFrameConn(t, path)
	defer winningStream.Close()
	claim := ReconnectClaim{AttachmentID: "attachment", PriorBindingID: oldReady.BindingID, PriorGeneration: 6, ProcessStart: identity.Start, StrongStart: identity.StrongStart, LastReceivedSeq: oldReadyFrame.Seq}
	writeTestFrame(t, winningStream, TypeReconnect, "winning-reconnect", 1, claim)
	winningReadyFrame := readTestFrame(t, winningStream)
	var winningReady Ready
	if err := winningReadyFrame.PayloadInto(&winningReady); err != nil {
		t.Fatal(err)
	}
	if winningReadyFrame.Type != TypeReady || winningReady.BindingID == oldReady.BindingID {
		t.Fatalf("winning reconnect = %#v", winningReadyFrame)
	}
	eventually(t, func() bool {
		bindings := broker.Bindings()
		return len(bindings) == 1 && bindings[0].BindingID == winningReady.BindingID
	})
	_ = oldStream.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := oldStream.ReadFrame(); err == nil {
		t.Fatal("replaced component stream remained readable")
	}
	if err := broker.Send(oldReady.BindingID, TypeSessionBound, "old-send", SessionBound{BindingID: oldReady.BindingID, AttachmentID: "attachment", NativeSessionID: "native", PublicName: "old"}); err == nil {
		t.Fatal("replaced binding remained routable")
	}

	staleStream := dialFrameConn(t, path)
	defer staleStream.Close()
	writeTestFrame(t, staleStream, TypeReconnect, "stale-reconnect", 1, claim)
	staleReject := readTestFrame(t, staleStream)
	var rejection Reject
	if err := staleReject.PayloadInto(&rejection); err != nil {
		t.Fatal(err)
	}
	if staleReject.Type != TypeReject || rejection.Category != CategoryReplay {
		t.Fatalf("stale dual-stream response = %#v / %#v", staleReject, rejection)
	}
	futureStream := dialFrameConn(t, path)
	defer futureStream.Close()
	futureClaim := claim
	futureClaim.PriorBindingID = winningReady.BindingID
	futureClaim.PriorGeneration = 7
	writeTestFrame(t, futureStream, TypeReconnect, "future-reconnect", 1, futureClaim)
	futureReject := readTestFrame(t, futureStream)
	if err := futureReject.PayloadInto(&rejection); err != nil {
		t.Fatal(err)
	}
	if futureReject.Type != TypeReject || rejection.Category != CategoryProtocol {
		t.Fatalf("future reconnect response = %#v / %#v", futureReject, rejection)
	}

	writeTestFrame(t, winningStream, TypeHeartbeat, "winner-heartbeat", 2, Heartbeat{BindingID: winningReady.BindingID, LastReceivedSeq: winningReadyFrame.Seq})
	if ack := readTestFrame(t, winningStream); ack.Type != TypeHeartbeatAck {
		t.Fatalf("winner heartbeat response = %s", ack.Type)
	}
	if authorizer.reconnects != 2 {
		t.Fatalf("authorizer reconnect calls = %d, want every attach re-attested before fencing", authorizer.reconnects)
	}
}

func TestBrokerHeartbeatTimeoutClosesBinding(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	broker, _ := NewBroker(Config{Generation: 1, Authorizer: authorizer, Handler: noopComponentHandler, HeartbeatInterval: 20 * time.Millisecond, HeartbeatGrace: 2})
	path, stop := serveTestBroker(t, broker)
	defer stop()
	connection := dialFrameConn(t, path)
	defer connection.Close()
	writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: ContractRevision})
	_ = readTestFrame(t, connection)
	time.Sleep(80 * time.Millisecond)
	if _, err := connection.ReadFrame(); err == nil {
		t.Fatal("connection survived missed heartbeat deadline")
	}
	eventually(t, func() bool { return len(broker.Bindings()) == 0 })
}

func TestBrokerRollsBackHandlerAdmissionForRetryAndOutstandingBounds(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	var mu sync.Mutex
	counts := make(map[string]int)
	handler := HandlerFunc(func(_ context.Context, _ BindingView, frame Frame) error {
		operationID := frame.ID
		if frame.Type == TypeDeliveryAccept {
			var value DeliveryAccept
			if err := frame.PayloadInto(&value); err != nil {
				return err
			}
			operationID = value.DeliveryID
		}
		mu.Lock()
		counts[string(frame.Type)+":"+operationID]++
		mu.Unlock()
		return errors.New("handler refused operation")
	})
	broker, err := NewBroker(Config{Generation: 9, Authorizer: authorizer, Handler: handler, MaxOutstanding: 1, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	connection := dialFrameConn(t, path)
	defer connection.Close()
	writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart, ComponentVersion: ContractRevision})
	readyFrame := readTestFrame(t, connection)
	var ready Ready
	if err := readyFrame.PayloadInto(&ready); err != nil {
		t.Fatal(err)
	}

	tool := ToolCall{CallID: "tool-one", Operation: "sessions.list", Arguments: json.RawMessage(`{}`)}
	writeTestFrame(t, connection, TypeToolCall, "tool-one", 2, tool)
	assertRejectFrame(t, connection, "tool-one")
	writeTestFrame(t, connection, TypeToolCall, "tool-one", 3, tool)
	assertRejectFrame(t, connection, "tool-one")
	writeTestFrame(t, connection, TypeToolCall, "tool-two", 4, ToolCall{CallID: "tool-two", Operation: "sessions.list", Arguments: json.RawMessage(`{}`)})
	assertRejectFrame(t, connection, "tool-two")

	if err := broker.Send(ready.BindingID, TypeDeliveryPresent, "delivery-one", DeliveryPresent{DeliveryID: "delivery-one", Mode: "wake", Body: json.RawMessage(`{"text":"hello"}`)}); err != nil {
		t.Fatal(err)
	}
	if present := readTestFrame(t, connection); present.Type != TypeDeliveryPresent {
		t.Fatalf("delivery frame = %s", present.Type)
	}
	accept := DeliveryAccept{DeliveryID: "delivery-one", NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 1}
	writeTestFrame(t, connection, TypeDeliveryAccept, "delivery-one", 5, accept)
	assertRejectFrame(t, connection, "delivery-one")
	writeTestFrame(t, connection, TypeDeliveryAccept, "delivery-one", 6, accept)
	assertRejectFrame(t, connection, "delivery-one")
	reject := DeliveryReject{DeliveryID: "delivery-one", Category: Category("native-rejected"), Detail: "refused"}
	writeTestFrame(t, connection, TypeDeliveryReject, "delivery-one", 7, reject)
	assertRejectFrame(t, connection, "delivery-one")
	writeTestFrame(t, connection, TypeDeliveryReject, "delivery-one", 8, reject)
	assertRejectFrame(t, connection, "delivery-one")

	mu.Lock()
	defer mu.Unlock()
	if counts["tool.call:tool-one"] != 2 || counts["tool.call:tool-two"] != 1 ||
		counts["delivery.accept:delivery-one"] != 2 || counts["delivery.reject:delivery-one"] != 2 {
		t.Fatalf("handler retry counts = %#v", counts)
	}
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
	return Authorization{BindingID: "binding-bootstrap", AttachmentID: claim.AttachmentID, ProductID: claim.ProductID, ProcessIdentity: a.process, BootstrapRevision: 1}, nil
}

func (a *testAuthorizer) Reconnect(_ context.Context, claim ReconnectClaim, evidence PeerEvidence) (Authorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconnects++
	return Authorization{BindingID: fmt.Sprintf("binding-reconnect-%d", a.reconnects), AttachmentID: claim.AttachmentID, ProductID: "opencode", ProcessIdentity: a.process, BootstrapRevision: 1}, nil
}

type durableHandshakeAuthorizer struct {
	mu           sync.Mutex
	secret       string
	process      procinfo.Identity
	attachmentID string
	productID    string
	capabilityID string
	revision     uint64
	current      string
	currentReady bool
	predecessor  string
	successor    string
	nextBinding  int
	successors   int
}

func (a *durableHandshakeAuthorizer) Bootstrap(_ context.Context, claim BootstrapClaim, evidence PeerEvidence) (Authorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if evidence.Process != a.process || claim.ProductID != a.productID || claim.AttachmentID != a.attachmentID ||
		claim.BootstrapCapabilityID != a.capabilityID || claim.BootstrapValue != a.secret ||
		claim.ProcessStart != a.process.Start || claim.StrongStart != a.process.StrongStart {
		return Authorization{}, &ProtocolError{Category: CategoryUnauthorized, Detail: "bootstrap evidence conflicts with durable binding"}
	}
	if a.currentReady {
		return Authorization{}, &ProtocolError{Category: CategoryReplay, Detail: "bootstrap binding was already adopted"}
	}
	if a.current == "" {
		a.current = a.allocateBinding()
	}
	return a.authorization(a.current), nil
}

func (a *durableHandshakeAuthorizer) Reconnect(_ context.Context, claim ReconnectClaim, evidence PeerEvidence) (Authorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if evidence.Process != a.process || claim.AttachmentID != a.attachmentID ||
		claim.ProcessStart != a.process.Start || claim.StrongStart != a.process.StrongStart || !a.currentReady {
		return Authorization{}, &ProtocolError{Category: CategoryUnauthorized, Detail: "reconnect evidence conflicts with durable binding"}
	}
	if a.successor != "" {
		if claim.PriorBindingID != a.predecessor {
			return Authorization{}, &ProtocolError{Category: CategoryReplay, Detail: "reconnect predecessor is stale"}
		}
		return a.authorization(a.successor), nil
	}
	if claim.PriorBindingID != a.current {
		return Authorization{}, &ProtocolError{Category: CategoryReplay, Detail: "reconnect predecessor is stale"}
	}
	a.predecessor = a.current
	a.successor = a.allocateBinding()
	a.successors++
	return a.authorization(a.successor), nil
}

func (a *durableHandshakeAuthorizer) adopt(bindingID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case bindingID == a.current && !a.currentReady:
		a.currentReady = true
		return nil
	case bindingID == a.successor && a.successor != "":
		a.current = a.successor
		a.currentReady = true
		a.predecessor = ""
		a.successor = ""
		return nil
	default:
		return &ProtocolError{Category: CategoryReplay, Detail: "binding adoption is stale"}
	}
}

func (a *durableHandshakeAuthorizer) isCurrent(bindingID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentReady && a.current == bindingID && a.successor == ""
}

func (a *durableHandshakeAuthorizer) successorCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.successors
}

func (a *durableHandshakeAuthorizer) allocateBinding() string {
	a.nextBinding++
	return fmt.Sprintf("binding-durable-%d", a.nextBinding)
}

func (a *durableHandshakeAuthorizer) authorization(bindingID string) Authorization {
	return Authorization{
		BindingID: bindingID, AttachmentID: a.attachmentID, ProductID: a.productID,
		ProcessIdentity: a.process, BootstrapRevision: a.revision,
	}
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

func assertRejectFrame(t *testing.T, connection *localtransport.Conn, operationID string) {
	t.Helper()
	frame := readTestFrame(t, connection)
	var rejection Reject
	if err := frame.PayloadInto(&rejection); err != nil {
		t.Fatal(err)
	}
	if frame.Type != TypeReject || rejection.OperationID != operationID || rejection.Category != CategoryProtocol {
		t.Fatalf("reject frame = %#v / %#v", frame, rejection)
	}
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
