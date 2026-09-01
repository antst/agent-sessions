package component

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestBrokerRenameRequestCorrelationReplayObservationAndFailures(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	handled := make(chan Frame, 4)
	releaseDurableRename := make(chan struct{})
	handlerFailureAttempts := 0
	broker, err := NewBroker(Config{
		Generation: 3, Authorizer: authorizer, HeartbeatInterval: time.Second,
		Handler: HandlerFunc(func(_ context.Context, _ BindingView, frame Frame) error {
			handled <- frame
			if frame.ID == DaemonRenameOperationPrefix+"stable-one" {
				<-releaseDurableRename
			}
			if frame.ID == DaemonRenameOperationPrefix+"handler-failure" {
				handlerFailureAttempts++
				if handlerFailureAttempts == 1 {
					return &ProtocolError{Category: CategoryInternal, Detail: "durable projection commit failed token=private"}
				}
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	connection := dialFrameConn(t, path)
	defer connection.Close()
	writeTestFrame(t, connection, TypeBootstrap, "bootstrap", 1, BootstrapClaim{
		ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability",
		BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart,
		ComponentVersion: ContractRevision,
	})
	readyFrame := readTestFrame(t, connection)
	var ready Ready
	if err := readyFrame.PayloadInto(&ready); err != nil {
		t.Fatal(err)
	}
	if err := broker.Send(ready.BindingID, TypeSessionRenameRequest, DaemonRenameOperationPrefix+"bypass", SessionRenameRequest{NativeSessionID: "native", RequestedName: "bypass"}); err == nil {
		t.Fatal("generic broker send bypassed rename correlation")
	}

	type renameCall struct {
		result SessionRename
		err    error
	}
	call := func(ctx context.Context, id, name string) <-chan renameCall {
		result := make(chan renameCall, 1)
		go func() {
			value, callErr := broker.RenameSession(ctx, ready.BindingID, id, SessionRenameRequest{NativeSessionID: "native", RequestedName: name})
			result <- renameCall{result: value, err: callErr}
		}()
		return result
	}

	operationID := DaemonRenameOperationPrefix + "stable-one"
	first := call(context.Background(), operationID, "renamed")
	requestFrame := readTestFrame(t, connection)
	if requestFrame.Type != TypeSessionRenameRequest || requestFrame.ID != operationID {
		t.Fatalf("rename request = %#v", requestFrame)
	}
	var request SessionRenameRequest
	if err := requestFrame.PayloadInto(&request); err != nil {
		t.Fatal(err)
	}
	if request.NativeSessionID != "native" || request.RequestedName != "renamed" {
		t.Fatalf("rename request payload = %#v", request)
	}
	writeTestFrame(t, connection, TypeSessionRename, operationID, 2, SessionRename{
		NativeSessionID: "native", NativeName: "renamed", ProductEventSeq: 2,
	})
	select {
	case durable := <-handled:
		if durable.ID != operationID {
			t.Fatalf("durable rename frame = %#v", durable)
		}
	case <-time.After(time.Second):
		t.Fatal("correlated rename did not reach durable handler")
	}
	select {
	case early := <-first:
		t.Fatalf("rename accepted before durable handler completed: %#v", early)
	default:
	}
	close(releaseDurableRename)
	if got := <-first; got.err != nil || got.result.NativeName != "renamed" {
		t.Fatalf("rename result = %#v / %v", got.result, got.err)
	}

	// Same id and body returns the exact bounded completed result without a
	// second product write. A changed body conflicts.
	if got := <-call(context.Background(), operationID, "renamed"); got.err != nil || got.result.ProductEventSeq != 2 {
		t.Fatalf("idempotent rename replay = %#v / %v", got.result, got.err)
	}
	assertRenameError(t, (<-call(context.Background(), operationID, "different")).err, RenameProtocol)

	// An observation occupies the component namespace and is routed to the
	// durable handler, never mistaken for a daemon response.
	observationID := ComponentRenameObservationPrefix + "native-event-3"
	writeTestFrame(t, connection, TypeSessionRename, observationID, 3, SessionRename{
		NativeSessionID: "native", NativeName: "externally-renamed", ProductEventSeq: 3,
	})
	select {
	case frame := <-handled:
		if frame.ID != observationID || frame.Type != TypeSessionRename {
			t.Fatalf("unsolicited rename = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("unsolicited rename did not reach durable handler")
	}

	// A daemon-space response without an outstanding request cannot collide
	// into the unsolicited-observation path.
	writeTestFrame(t, connection, TypeSessionRename, DaemonRenameOperationPrefix+"unknown", 4, SessionRename{
		NativeSessionID: "native", NativeName: "wrong", ProductEventSeq: 4,
	})
	assertRejectFrame(t, connection, DaemonRenameOperationPrefix+"unknown")

	mismatchID := DaemonRenameOperationPrefix + "mismatch"
	mismatch := call(context.Background(), mismatchID, "expected")
	if frame := readTestFrame(t, connection); frame.Type != TypeSessionRenameRequest || frame.ID != mismatchID {
		t.Fatalf("mismatch request = %#v", frame)
	}
	writeTestFrame(t, connection, TypeSessionRename, mismatchID, 5, SessionRename{
		NativeSessionID: "native", NativeName: "not-expected", ProductEventSeq: 5,
	})
	assertRejectFrame(t, connection, mismatchID)
	assertRenameError(t, (<-mismatch).err, RenameProtocol)

	handlerFailureID := DaemonRenameOperationPrefix + "handler-failure"
	handlerFailure := call(context.Background(), handlerFailureID, "handler-failure")
	if frame := readTestFrame(t, connection); frame.Type != TypeSessionRenameRequest || frame.ID != handlerFailureID {
		t.Fatalf("handler failure request = %#v", frame)
	}
	writeTestFrame(t, connection, TypeSessionRename, handlerFailureID, 6, SessionRename{
		NativeSessionID: "native", NativeName: "handler-failure", ProductEventSeq: 6,
	})
	select {
	case durable := <-handled:
		if durable.ID != handlerFailureID {
			t.Fatalf("handler failure durable frame = %#v", durable)
		}
	case <-time.After(time.Second):
		t.Fatal("handler failure did not reach durable handler")
	}
	assertRenameRejectFrame(t, connection, handlerFailureID, CategoryInternal)
	handlerFailureResult := <-handlerFailure
	assertRenameError(t, handlerFailureResult.err, RenameUnavailable)
	if strings.Contains(handlerFailureResult.err.Error(), "private") {
		t.Fatalf("handler failure leaked redacted detail: %q", handlerFailureResult.err)
	}
	// A transient durable-handler failure does not poison the stable operation.
	// Retrying sends the same request, the component replays its exact native
	// confirmation, and the handler converges before acceptance.
	handlerRetry := call(context.Background(), handlerFailureID, "handler-failure")
	if frame := readTestFrame(t, connection); frame.Type != TypeSessionRenameRequest || frame.ID != handlerFailureID {
		t.Fatalf("handler retry request = %#v", frame)
	}
	writeTestFrame(t, connection, TypeSessionRename, handlerFailureID, 7, SessionRename{
		NativeSessionID: "native", NativeName: "handler-failure", ProductEventSeq: 6,
	})
	select {
	case durable := <-handled:
		if durable.ID != handlerFailureID {
			t.Fatalf("handler retry durable frame = %#v", durable)
		}
	case <-time.After(time.Second):
		t.Fatal("handler retry did not reach durable handler")
	}
	if result := <-handlerRetry; result.err != nil || result.result.NativeName != "handler-failure" {
		t.Fatalf("handler retry result = %#v / %v", result.result, result.err)
	}

	unsupportedID := DaemonRenameOperationPrefix + "unsupported"
	unsupported := call(context.Background(), unsupportedID, "unsupported")
	if frame := readTestFrame(t, connection); frame.Type != TypeSessionRenameRequest || frame.ID != unsupportedID {
		t.Fatalf("unsupported request = %#v", frame)
	}
	writeTestFrame(t, connection, TypeReject, unsupportedID, 8, Reject{
		OperationID: unsupportedID, Category: CategoryUnknownType, Detail: "older pinned client does not know frame token=private",
	})
	unsupportedResult := <-unsupported
	assertRenameError(t, unsupportedResult.err, RenameUnsupported)
	if strings.Contains(unsupportedResult.err.Error(), "private") {
		t.Fatalf("rename error leaked redacted diagnostic: %q", unsupportedResult.err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	timedOutID := DaemonRenameOperationPrefix + "timeout"
	timedOut := call(ctx, timedOutID, "timeout")
	if frame := readTestFrame(t, connection); frame.Type != TypeSessionRenameRequest || frame.ID != timedOutID {
		t.Fatalf("timeout request = %#v", frame)
	}
	assertRenameError(t, (<-timedOut).err, RenameTimedOut)

	unavailableID := DaemonRenameOperationPrefix + "disconnect"
	unavailable := call(context.Background(), unavailableID, "disconnect")
	if frame := readTestFrame(t, connection); frame.Type != TypeSessionRenameRequest || frame.ID != unavailableID {
		t.Fatalf("disconnect request = %#v", frame)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	assertRenameError(t, (<-unavailable).err, RenameUnavailable)
}

func TestBrokerRenameOutstandingBound(t *testing.T) {
	tracker := newRenameTracker(1, 2)
	first, created, err := tracker.reserve(DaemonRenameOperationPrefix+"one", SessionRenameRequest{NativeSessionID: "native", RequestedName: "one"})
	if err != nil || !created || first == nil {
		t.Fatalf("first reservation = %#v / %t / %v", first, created, err)
	}
	if _, _, err := tracker.reserve(DaemonRenameOperationPrefix+"two", SessionRenameRequest{NativeSessionID: "native", RequestedName: "two"}); err == nil {
		t.Fatal("rename tracker widened its outstanding bound")
	}
	tracker.close()

	completed := newRenameTracker(1, 2)
	for index, name := range []string{"one", "two", "three", "four"} {
		operationID := DaemonRenameOperationPrefix + name
		if _, created, err := completed.reserve(operationID, SessionRenameRequest{NativeSessionID: "native", RequestedName: name}); err != nil || !created {
			t.Fatalf("reserve completed %d = %t / %v", index, created, err)
		}
		frame, err := NewFrame(TypeSessionRename, operationID, uint64(index+1), SessionRename{NativeSessionID: "native", NativeName: name, ProductEventSeq: uint64(index + 1)})
		if err != nil {
			t.Fatal(err)
		}
		operation, duplicate, err := completed.admitResponse(frame)
		if err != nil || duplicate {
			t.Fatalf("admit completed response = %t / %v", duplicate, err)
		}
		if err := completed.commitResponse(operationID, operation); err != nil {
			t.Fatal(err)
		}
	}
	completed.mu.Lock()
	defer completed.mu.Unlock()
	if len(completed.completed) != 2 || len(completed.completedOrder) != 2 {
		t.Fatalf("completed rename replay bound = %d / %d", len(completed.completed), len(completed.completedOrder))
	}
}

func TestBrokerRejectsMismatchedPinnedContractBeforeAuthentication(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &testAuthorizer{secret: "bootstrap", process: identity}
	broker, err := NewBroker(Config{Generation: 1, Authorizer: authorizer, Handler: noopComponentHandler, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	path, stop := serveTestBroker(t, broker)
	defer stop()
	connection := dialFrameConn(t, path)
	defer connection.Close()
	stale, err := NewFrame(TypeBootstrap, "stale-contract", 1, BootstrapClaim{
		ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability",
		BootstrapValue: authorizer.secret, ProcessStart: identity.Start, StrongStart: identity.StrongStart,
		ComponentVersion: "agent-sessions.component.v1-r0",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteFrame(body); err != nil {
		t.Fatal(err)
	}
	frame := readTestFrame(t, connection)
	var rejection Reject
	if err := frame.PayloadInto(&rejection); err != nil {
		t.Fatal(err)
	}
	if frame.Type != TypeReject || rejection.Category != CategoryUnsupportedVersion {
		t.Fatalf("mismatched contract response = %#v / %#v", frame, rejection)
	}
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	if authorizer.used {
		t.Fatal("mismatched pinned component reached bootstrap authentication")
	}
}

func assertRenameError(t *testing.T, err error, category RenameFailureCategory) {
	t.Helper()
	var renameErr *RenameError
	if !errors.As(err, &renameErr) || renameErr.Category != category {
		t.Fatalf("rename error = %#v, want category %s", err, category)
	}
}

func assertRenameRejectFrame(t *testing.T, connection interface{ ReadFrame() ([]byte, error) }, operationID string, category Category) {
	t.Helper()
	body, err := connection.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := DecodeFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	var rejection Reject
	if err := frame.PayloadInto(&rejection); err != nil {
		t.Fatal(err)
	}
	if frame.Type != TypeReject || rejection.OperationID != operationID || rejection.Category != category {
		t.Fatalf("rename reject frame = %#v / %#v", frame, rejection)
	}
}
