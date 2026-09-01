package pifamily

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type bindingFixture struct{ values []component.BindingView }

func (source *bindingFixture) Bindings() []component.BindingView {
	return append([]component.BindingView(nil), source.values...)
}

type componentLookupCall struct {
	attachmentID    string
	nativeSessionID string
}

type recordingComponentLookup struct {
	view  productruntime.ComponentSessionView
	calls []componentLookupCall
}

func (lookup *recordingComponentLookup) LookupComponent(
	_ context.Context,
	attachmentID string,
	nativeSessionID string,
) (productruntime.ComponentSessionView, error) {
	lookup.calls = append(lookup.calls, componentLookupCall{
		attachmentID: attachmentID, nativeSessionID: nativeSessionID,
	})
	return lookup.view, nil
}

type sentComponentFrame struct {
	bindingID   string
	typeName    component.FrameType
	operationID string
	payload     any
}

type recordingSender struct {
	mu             sync.Mutex
	frames         []sentComponentFrame
	sent           chan sentComponentFrame
	renameRequests []component.SessionRenameRequest
	renameErr      error
	sendErrors     []error
}

func newRecordingSender() *recordingSender {
	return &recordingSender{sent: make(chan sentComponentFrame, 16)}
}
func (sender *recordingSender) Send(bindingID string, typeName component.FrameType, operationID string, payload any) error {
	frame := sentComponentFrame{bindingID: bindingID, typeName: typeName, operationID: operationID, payload: payload}
	sender.mu.Lock()
	if len(sender.sendErrors) > 0 {
		err := sender.sendErrors[0]
		sender.sendErrors = sender.sendErrors[1:]
		sender.mu.Unlock()
		return err
	}
	sender.frames = append(sender.frames, frame)
	sender.mu.Unlock()
	sender.sent <- frame
	return nil
}

type parentToolHandlerFunc func(context.Context, ParentToolCall) (json.RawMessage, error)

func (handler parentToolHandlerFunc) HandleParentTool(ctx context.Context, call ParentToolCall) (json.RawMessage, error) {
	return handler(ctx, call)
}
func (sender *recordingSender) RenameSession(_ context.Context, bindingID, operationID string, request component.SessionRenameRequest) (component.SessionRename, error) {
	sender.mu.Lock()
	sender.renameRequests = append(sender.renameRequests, request)
	sender.mu.Unlock()
	if sender.renameErr != nil {
		return component.SessionRename{}, sender.renameErr
	}
	if bindingID == "" || operationID == "" {
		return component.SessionRename{}, errors.New("incomplete rename")
	}
	return component.SessionRename{NativeSessionID: request.NativeSessionID, NativeName: request.RequestedName, ProductEventSeq: 2}, nil
}

func testComponentRuntime(t *testing.T, productID string, binding component.BindingView) (*ComponentRuntime, *recordingSender, *bindingFixture) {
	t.Helper()
	quirks, _ := QuirksFor(productID)
	sender := newRecordingSender()
	bindings := &bindingFixture{values: []component.BindingView{binding}}
	runtime, err := NewComponentRuntime(ComponentRuntimeConfig{Quirks: quirks, Sender: sender, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.recordIdentity(binding, "native-1", 1); err != nil {
		t.Fatal(err)
	}
	return runtime, sender, bindings
}

func TestComponentDeliveryCorrelatesExactNativeAcceptance(t *testing.T) {
	binding := component.BindingView{BindingID: "binding-1", AttachmentID: "attachment-1", ProductID: PiProductID, Generation: 9}
	runtime, sender, _ := testComponentRuntime(t, PiProductID, binding)
	attachment := daemon.ManagedAttachment{ID: "attachment-1", Product: PiProductID, NativeSessionID: "native-1", State: "attached"}
	done := make(chan struct {
		accept productruntime.NativeAcceptance
		err    error
	}, 1)
	go func() {
		accept, err := runtime.Deliver(context.Background(), attachment, productruntime.DeliveryRequest{
			DeliveryID: "delivery-1", ReceiptID: "receipt-1", Mode: productruntime.DeliveryIdleWake,
			Body: []byte(`{"version":1,"type":"delivery","message_id":"message-1","content":"hello"}`),
		})
		done <- struct {
			accept productruntime.NativeAcceptance
			err    error
		}{accept: accept, err: err}
	}()
	present := <-sender.sent
	if present.bindingID != "binding-1" || present.typeName != component.TypeDeliveryPresent || present.operationID != "delivery-1" {
		t.Fatalf("present = %+v", present)
	}
	frame, err := component.NewFrame(component.TypeDeliveryAccept, "delivery-1", 1, component.DeliveryAccept{
		DeliveryID: "delivery-1", NativeSessionID: "native-1", NativeMessageID: "message-1", AcceptedAt: 1700000000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.HandleComponentFrame(context.Background(), binding, frame); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || result.accept.NativeSessionID != "native-1" || result.accept.NativeMessageID != "message-1" || !result.accept.AcceptedAt.Equal(time.UnixMilli(1700000000000).UTC()) {
		t.Fatalf("delivery result = %+v", result)
	}
}

func TestComponentRejectsForeignAcceptanceAndUsesBrokerCorrelatedRename(t *testing.T) {
	binding := component.BindingView{BindingID: "binding-1", AttachmentID: "attachment-1", ProductID: PiProductID, Generation: 9}
	runtime, sender, _ := testComponentRuntime(t, PiProductID, binding)
	attachment := daemon.ManagedAttachment{ID: "attachment-1", Product: PiProductID, NativeSessionID: "native-1", State: "attached"}
	deliveryDone := make(chan error, 1)
	go func() {
		_, err := runtime.Deliver(context.Background(), attachment, productruntime.DeliveryRequest{
			DeliveryID: "delivery-foreign", Mode: productruntime.DeliveryBusySteer,
			Body: []byte(`{"version":1,"type":"delivery","message_id":"message-1","content":"hello"}`),
		})
		deliveryDone <- err
	}()
	<-sender.sent
	frame, _ := component.NewFrame(component.TypeDeliveryAccept, "delivery-foreign", 1, component.DeliveryAccept{
		DeliveryID: "delivery-foreign", NativeSessionID: "forged-native", NativeMessageID: "message-1", AcceptedAt: 1,
	})
	if err := runtime.HandleComponentFrame(context.Background(), binding, frame); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("foreign acceptance returned %v", err)
	}
	select {
	case <-deliveryDone:
		t.Fatal("foreign acceptance completed the pending delivery")
	case <-time.After(20 * time.Millisecond):
	}
	validFrame, _ := component.NewFrame(component.TypeDeliveryAccept, "delivery-foreign", 2, component.DeliveryAccept{
		DeliveryID: "delivery-foreign", NativeSessionID: "native-1", NativeMessageID: "message-1", AcceptedAt: 2,
	})
	if err := runtime.HandleComponentFrame(context.Background(), binding, validFrame); err != nil {
		t.Fatalf("valid acceptance after hostile attempt: %v", err)
	}
	if err := <-deliveryDone; err != nil {
		t.Fatalf("valid acceptance returned %v", err)
	}
	renameCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	renamed, err := runtime.Rename(renameCtx, attachment, "new name")
	if err != nil || renamed != (productruntime.NativeName{Applied: "new name", NativeConfirmed: true}) {
		t.Fatalf("broker-correlated rename = %+v, %v", renamed, err)
	}
	sender.mu.Lock()
	renames := append([]component.SessionRenameRequest(nil), sender.renameRequests...)
	sender.mu.Unlock()
	if !reflect.DeepEqual(renames, []component.SessionRenameRequest{{NativeSessionID: "native-1", RequestedName: "new name"}}) {
		t.Fatalf("rename requests = %+v", renames)
	}
}

func TestCorrelatedDeliveryFrameIDMismatchCannotMutatePendingOperation(t *testing.T) {
	for _, frameType := range []component.FrameType{component.TypeDeliveryAccept, component.TypeDeliveryReject} {
		t.Run(string(frameType), func(t *testing.T) {
			binding := component.BindingView{BindingID: "binding-id", AttachmentID: "attachment-id", ProductID: PiProductID, Generation: 2}
			runtime, sender, _ := testComponentRuntime(t, PiProductID, binding)
			attachment := daemon.ManagedAttachment{ID: binding.AttachmentID, Product: PiProductID, NativeSessionID: "native-1", State: "attached"}
			done := make(chan error, 1)
			go func() {
				_, err := runtime.Deliver(context.Background(), attachment, productruntime.DeliveryRequest{
					DeliveryID: "delivery-exact", Mode: productruntime.DeliveryIdleWake,
					Body: []byte(`{"version":1,"type":"delivery"}`),
				})
				done <- err
			}()
			<-sender.sent

			var payload any = component.DeliveryAccept{
				DeliveryID: "delivery-exact", NativeSessionID: "native-1", NativeMessageID: "message", AcceptedAt: 1,
			}
			if frameType == component.TypeDeliveryReject {
				payload = component.DeliveryReject{DeliveryID: "delivery-exact", Category: component.CategoryProtocol, Detail: "rejected"}
			}
			mismatch, _ := component.NewFrame(frameType, "different-operation", 1, payload)
			if err := runtime.HandleComponentFrame(context.Background(), binding, mismatch); !errors.Is(err, productruntime.ErrProtocol) {
				t.Fatalf("mismatched frame returned %v", err)
			}
			select {
			case <-done:
				t.Fatal("mismatched frame mutated the pending delivery")
			case <-time.After(20 * time.Millisecond):
			}

			exact, _ := component.NewFrame(frameType, "delivery-exact", 2, payload)
			if err := runtime.HandleComponentFrame(context.Background(), binding, exact); err != nil {
				t.Fatal(err)
			}
			resultErr := <-done
			if frameType == component.TypeDeliveryAccept && resultErr != nil {
				t.Fatalf("exact acceptance returned %v", resultErr)
			}
			if frameType == component.TypeDeliveryReject && !errors.Is(resultErr, productruntime.ErrNativeRejected) {
				t.Fatalf("exact rejection returned %v", resultErr)
			}
		})
	}
}

func TestParentToolOversizeAndSendFailureProduceBoundedFailureResult(t *testing.T) {
	binding := component.BindingView{BindingID: "binding-tool", AttachmentID: "attachment-tool", ProductID: PiProductID, Generation: 3}
	for name, fixture := range map[string]struct {
		result     json.RawMessage
		sendErrors []error
		category   component.Category
	}{
		"oversized valid JSON": {
			result:   json.RawMessage(`{"value":"` + strings.Repeat("x", maxParentToolResultBytes) + `"}`),
			category: component.CategoryProtocol,
		},
		"initial send failure": {
			result: json.RawMessage(`{"ok":true}`), sendErrors: []error{errors.New("send rejected")},
			category: component.CategoryInternal,
		},
	} {
		t.Run(name, func(t *testing.T) {
			quirks, _ := QuirksFor(PiProductID)
			sender := newRecordingSender()
			sender.sendErrors = append([]error(nil), fixture.sendErrors...)
			bindings := &bindingFixture{values: []component.BindingView{binding}}
			runtime, err := NewComponentRuntime(ComponentRuntimeConfig{
				Quirks: quirks, Sender: sender, Bindings: bindings,
				Tools: parentToolHandlerFunc(func(context.Context, ParentToolCall) (json.RawMessage, error) {
					return fixture.result, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.recordIdentity(binding, "native-tool", 1); err != nil {
				t.Fatal(err)
			}
			frame, err := component.NewFrame(component.TypeToolCall, "call-1", 2, component.ToolCall{
				CallID: "call-1", Operation: "peers.list", Arguments: json.RawMessage(`{}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.HandleComponentFrame(context.Background(), binding, frame); err != nil {
				t.Fatal(err)
			}
			select {
			case sent := <-sender.sent:
				result, ok := sent.payload.(component.ToolResult)
				if !ok || sent.typeName != component.TypeToolResult || result.CallID != "call-1" || result.Success || result.Category != fixture.category {
					t.Fatalf("bounded tool result = %+v", sent)
				}
				encoded, err := json.Marshal(result)
				if err != nil || len(encoded) > 1024 {
					t.Fatalf("failure result size = %d, error = %v", len(encoded), err)
				}
			case <-time.After(time.Second):
				t.Fatal("bounded tool failure result was dropped")
			}
		})
	}
}

func TestCorrelatedToolFrameIDMismatchCannotDispatchOrCancel(t *testing.T) {
	binding := component.BindingView{BindingID: "binding-tool-id", AttachmentID: "attachment-tool-id", ProductID: PiProductID, Generation: 4}
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	quirks, _ := QuirksFor(PiProductID)
	sender := newRecordingSender()
	runtime, err := NewComponentRuntime(ComponentRuntimeConfig{
		Quirks: quirks, Sender: sender, Bindings: &bindingFixture{values: []component.BindingView{binding}},
		Tools: parentToolHandlerFunc(func(ctx context.Context, _ ParentToolCall) (json.RawMessage, error) {
			started <- struct{}{}
			<-ctx.Done()
			cancelled <- struct{}{}
			return nil, ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.recordIdentity(binding, "native-tool", 1); err != nil {
		t.Fatal(err)
	}
	callPayload := component.ToolCall{CallID: "call-exact", Operation: "peers.list", Arguments: json.RawMessage(`{}`)}
	mismatchCall, _ := component.NewFrame(component.TypeToolCall, "different-call", 1, callPayload)
	if err := runtime.HandleComponentFrame(context.Background(), binding, mismatchCall); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("mismatched tool call returned %v", err)
	}
	select {
	case <-started:
		t.Fatal("mismatched tool call reached the handler")
	case <-time.After(20 * time.Millisecond):
	}

	exactCall, _ := component.NewFrame(component.TypeToolCall, "call-exact", 2, callPayload)
	if err := runtime.HandleComponentFrame(context.Background(), binding, exactCall); err != nil {
		t.Fatal(err)
	}
	<-started
	mismatchCancel, _ := component.NewFrame(component.TypeToolCancel, "different-call", 3, component.ToolCancel{CallID: "call-exact"})
	if err := runtime.HandleComponentFrame(context.Background(), binding, mismatchCancel); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("mismatched tool cancellation returned %v", err)
	}
	select {
	case <-cancelled:
		t.Fatal("mismatched tool cancellation cancelled the exact call")
	case <-time.After(20 * time.Millisecond):
	}
	exactCancel, _ := component.NewFrame(component.TypeToolCancel, "call-exact", 4, component.ToolCancel{CallID: "call-exact"})
	if err := runtime.HandleComponentFrame(context.Background(), binding, exactCancel); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("exact cancellation did not reach the handler")
	}
}

type processInspectorFixture struct {
	live        []procinfo.Identity
	directChild procinfo.Identity
	depth       int
	ancestor    procinfo.Identity
}

func (fixture *processInspectorFixture) CaptureIdentity(context.Context, int) (procinfo.Identity, error) {
	return fixture.live[0], nil
}
func (fixture *processInspectorFixture) ObserveIdentity(_ context.Context, identity procinfo.Identity) (procinfo.IdentityObservation, error) {
	for _, live := range fixture.live {
		if identity == live {
			return procinfo.IdentityObservation{Status: procinfo.IdentityMatches, Current: identity}, nil
		}
	}
	return procinfo.IdentityObservation{Status: procinfo.IdentityStale}, nil
}
func (*processInspectorFixture) Executable(context.Context, procinfo.Identity) (string, error) {
	return "/usr/bin/pi", nil
}
func (fixture *processInspectorFixture) DescendsFrom(_ context.Context, child, ancestor procinfo.Identity, depth int) (bool, error) {
	fixture.depth, fixture.ancestor = depth, ancestor
	return (child == ancestor || child == fixture.directChild) && ancestor == fixture.live[0] && depth == 1, nil
}

func TestParentAttesterRequiresExactRegisteredComponentProcessAndSession(t *testing.T) {
	for _, productID := range []string{PiProductID, OMPProductID} {
		t.Run(productID, func(t *testing.T) {
			process := procinfo.Identity{PID: 42, Start: productID + "-start", StrongStart: productID + "-strong"}
			connector := procinfo.Identity{PID: 43, Start: productID + "-connector", StrongStart: productID + "-connector-strong"}
			subagentChild := procinfo.Identity{PID: 44, Start: productID + "-subagent", StrongStart: productID + "-subagent-strong"}
			peer := localtransport.PeerIdentity{PID: process.PID, UID: 1000}
			binding := component.BindingView{
				BindingID: "binding-parent-" + productID, AttachmentID: "attachment-parent-" + productID,
				ProductID: productID, ProcessIdentity: process, PeerIdentity: peer, Generation: 4,
			}
			runtime, _, bindings := testComponentRuntime(t, productID, binding)
			inspector := &processInspectorFixture{live: []procinfo.Identity{process, connector, subagentChild}, directChild: connector}
			quirks, err := QuirksFor(productID)
			if err != nil {
				t.Fatal(err)
			}
			attester, err := NewParentAttester(quirks, runtime, bindings, inspector)
			if err != nil {
				t.Fatal(err)
			}
			attempt := productruntime.ConnectorAttempt{
				ProductID:       productID,
				PeerCredential:  localtransport.PeerIdentity{PID: connector.PID, UID: peer.UID},
				ProcessIdentity: connector, ClaimedNativeSessionID: "native-1",
				ComponentBindingID: binding.BindingID,
			}
			parent, err := attester.Attest(context.Background(), attempt)
			if err != nil {
				t.Fatal(err)
			}
			want := productruntime.ParentBinding{AttachmentID: binding.AttachmentID, NativeSessionID: "native-1", Verified: true}
			if parent != want || inspector.depth != 1 || inspector.ancestor != process {
				t.Fatalf("parent = %+v, want %+v, depth=%d ancestor=%+v", parent, want, inspector.depth, inspector.ancestor)
			}
			for name, mutate := range map[string]func(*productruntime.ConnectorAttempt){
				"false session": func(value *productruntime.ConnectorAttempt) {
					value.ClaimedNativeSessionID = "forged"
				},
				"foreign binding": func(value *productruntime.ConnectorAttempt) {
					value.ComponentBindingID = "binding-other"
				},
				"subagent process": func(value *productruntime.ConnectorAttempt) {
					value.ProcessIdentity = subagentChild
					value.PeerCredential.PID = subagentChild.PID
				},
			} {
				t.Run(name, func(t *testing.T) {
					forged := attempt
					mutate(&forged)
					if _, err := attester.Attest(context.Background(), forged); !errors.Is(err, productruntime.ErrUnauthorized) {
						t.Fatalf("forged attempt returned %v", err)
					}
				})
			}
		})
	}
}

func TestAttachmentAdapterLooksUpComponentByAttachmentAndNativeSession(t *testing.T) {
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	binding := component.BindingView{
		BindingID: "binding-lookup", AttachmentID: "attachment-lookup",
		ProductID: PiProductID, ProcessIdentity: process, Generation: 9,
	}
	runtime, _, _ := testComponentRuntime(t, PiProductID, binding)
	quirks, err := QuirksFor(PiProductID)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := NewPeerDriver(PeerConfig{
		Quirks: quirks, ExtensionPath: "/managed/agent-sessions.mjs",
		ComponentSocket: "/runtime/agent-sessions/component.sock", Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := &recordingComponentLookup{view: productruntime.ComponentSessionView{
		BindingID: "binding-lookup", AttachmentID: "attachment-lookup",
		NativeSessionID: "native-lookup", Generation: 9,
	}}
	inspector := &processInspectorFixture{live: []procinfo.Identity{process}}
	adapter, err := driver.AttachmentAdapter(productruntime.HostDeps{Components: lookup, Processes: inspector})
	if err != nil {
		t.Fatal(err)
	}
	attachment := daemon.ManagedAttachment{
		ID: "attachment-lookup", Product: PiProductID, NativeSessionID: "native-lookup",
		State: "attached", DaemonGeneration: 9,
	}
	evidence, err := adapter.Adopt(context.Background(), attachment, daemon.NativeEvidence{Process: process})
	if err != nil {
		t.Fatal(err)
	}
	attachment.Evidence = evidence
	if _, err := adapter.Refresh(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	want := []componentLookupCall{
		{attachmentID: "attachment-lookup", nativeSessionID: "native-lookup"},
		{attachmentID: "attachment-lookup", nativeSessionID: "native-lookup"},
	}
	if !reflect.DeepEqual(lookup.calls, want) {
		t.Fatalf("component lookup calls = %+v, want %+v", lookup.calls, want)
	}
}

func TestPeerLaunchInjectsManagedExtensionAndLiveComponentIdentity(t *testing.T) {
	for _, productID := range []string{PiProductID, OMPProductID} {
		t.Run(productID, func(t *testing.T) {
			binding := component.BindingView{BindingID: "binding", AttachmentID: "attachment", ProductID: productID, Generation: 1}
			runtime, _, _ := testComponentRuntime(t, productID, binding)
			quirks, _ := QuirksFor(productID)
			driver, err := NewPeerDriver(PeerConfig{
				Quirks: quirks, ExtensionPath: "/managed/agent-sessions.mjs",
				ComponentSocket: "/runtime/agent-sessions/component.sock", Runtime: runtime,
			})
			if err != nil {
				t.Fatal(err)
			}
			command, err := driver.BuildLaunch(context.Background(), productruntime.PeerLaunchRequest{
				ProductID: productID, AttachmentID: "attachment", Cwd: "/work", Args: []string{"--model", "fixture"},
				Env: []productruntime.EnvVar{{Name: "TERM", Value: "xterm"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			wantPrefix := quirks.extensionArguments("/managed/agent-sessions.mjs")
			if !reflect.DeepEqual(command.Args[:len(wantPrefix)], wantPrefix) || command.Cwd != "/work" {
				t.Fatalf("command = %+v", command)
			}
			if len(command.SensitiveEnv) != 0 {
				t.Fatalf("sensitive env = %+v", command.SensitiveEnv)
			}
			componentRevision, socket, attachment := "", "", ""
			for _, variable := range command.Env {
				switch variable.Name {
				case EnvComponentVersion:
					componentRevision = variable.Value
				case EnvComponentSocket:
					socket = variable.Value
				case EnvAttachmentID:
					attachment = variable.Value
				}
			}
			if componentRevision != component.ContractRevision || componentRevision == IntegrationVersion {
				t.Fatalf("component bootstrap revision = %q, product integration revision = %q", componentRevision, IntegrationVersion)
			}
			if socket != "/runtime/agent-sessions/component.sock" || attachment != "attachment" {
				t.Fatalf("live component environment = %+v", command.Env)
			}
			for _, argument := range []string{"--extension=/tmp/foreign", "--session-id=forged", "--approval-mode=yolo"} {
				request := productruntime.PeerLaunchRequest{
					ProductID: productID, AttachmentID: "attachment", Cwd: "/work", Args: []string{argument},
				}
				if _, err := driver.BuildLaunch(context.Background(), request); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
					t.Fatalf("reserved %q returned %v", argument, err)
				}
			}
		})
	}
}

var _ os.FileInfo = fileInfoFixture{}

type fileInfoFixture struct{}

func (fileInfoFixture) Name() string       { return filepath.Base("extension.mjs") }
func (fileInfoFixture) Size() int64        { return 1 }
func (fileInfoFixture) Mode() os.FileMode  { return 0444 }
func (fileInfoFixture) ModTime() time.Time { return time.Time{} }
func (fileInfoFixture) IsDir() bool        { return false }
func (fileInfoFixture) Sys() any           { return nil }
