package opencodefamily

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type componentLookupFake struct {
	view productruntime.ComponentSessionView
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

type reconnectingLookup struct {
	mu   sync.Mutex
	view productruntime.ComponentSessionView
}

func (lookup *reconnectingLookup) LookupComponent(_ context.Context, attachmentID, nativeID string) (productruntime.ComponentSessionView, error) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	if lookup.view.AttachmentID != attachmentID || lookup.view.NativeSessionID != nativeID {
		return productruntime.ComponentSessionView{}, productruntime.ErrStale
	}
	return lookup.view, nil
}

func (lookup *reconnectingLookup) set(view productruntime.ComponentSessionView) {
	lookup.mu.Lock()
	lookup.view = view
	lookup.mu.Unlock()
}

type renameGatewayFake struct {
	mu      sync.Mutex
	result  productruntime.NativeName
	binding string
	native  string
	name    string
}

func (*renameGatewayFake) Deliver(context.Context, string, string, productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrStale
}

func (gateway *renameGatewayFake) Rename(_ context.Context, binding, native, name string) (productruntime.NativeName, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.binding, gateway.native, gateway.name = binding, native, name
	return gateway.result, nil
}

func (lookup componentLookupFake) LookupComponent(_ context.Context, attachmentID, nativeID string) (productruntime.ComponentSessionView, error) {
	if lookup.view.AttachmentID != attachmentID || lookup.view.NativeSessionID != nativeID {
		return productruntime.ComponentSessionView{}, productruntime.ErrStale
	}
	return lookup.view, nil
}

type processInspectorFake struct {
	identity   procinfo.Identity
	executable string
}

func (inspector processInspectorFake) CaptureIdentity(context.Context, int) (procinfo.Identity, error) {
	return inspector.identity, nil
}
func (inspector processInspectorFake) ObserveIdentity(context.Context, procinfo.Identity) (procinfo.IdentityObservation, error) {
	return procinfo.IdentityObservation{Status: procinfo.IdentityMatches, Current: inspector.identity}, nil
}
func (inspector processInspectorFake) Executable(context.Context, procinfo.Identity) (string, error) {
	return inspector.executable, nil
}
func (inspector processInspectorFake) DescendsFrom(context.Context, procinfo.Identity, procinfo.Identity, int) (bool, error) {
	return true, nil
}

type gatewayFake struct {
	accepted productruntime.NativeAcceptance
	renamed  productruntime.NativeName
}

func TestComponentEnvReportsOnlyLiveComponentIdentity(t *testing.T) {
	request := productruntime.PeerLaunchRequest{
		ProductID: "opencode", AttachmentID: "attachment-live",
	}
	environment, sensitive := ComponentEnv(request)
	got := make(map[string]string, len(environment))
	for _, variable := range environment {
		if variable.Name == "AGENT_SESSIONS_PROCESS_START" || variable.Name == "AGENT_SESSIONS_STRONG_START" {
			t.Fatalf("component environment exported process declaration %q", variable.Name)
		}
		got[variable.Name] = variable.Value
	}
	if len(got) != 3 || got["AGENT_SESSIONS_PRODUCT_ID"] != "opencode" ||
		got["AGENT_SESSIONS_ATTACHMENT_ID"] != "attachment-live" ||
		got["AGENT_SESSIONS_COMPONENT_VERSION"] != ComponentProtocol {
		t.Fatalf("component environment = %#v", got)
	}
	if len(sensitive) != 0 {
		t.Fatalf("component sensitive environment = %#v", sensitive)
	}
}

func (gateway gatewayFake) Deliver(context.Context, string, string, productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	return gateway.accepted, nil
}
func (gateway gatewayFake) Rename(context.Context, string, string, string) (productruntime.NativeName, error) {
	return gateway.renamed, nil
}

func TestComponentPeerRequiresExactLiveBindingAndNativeAcceptance(t *testing.T) {
	process := procinfo.Identity{PID: 123, Start: "start", StrongStart: "strong"}
	view := productruntime.ComponentSessionView{BindingID: "binding-one", AttachmentID: "attachment-one", NativeSessionID: "ses_one", Generation: 9, State: "idle"}
	acceptedAt := time.Now().Add(-time.Second)
	driver, err := NewPeerDriver(PeerConfig{
		ProductID: "opencode", Executable: "opencode", IntegrationVersion: "1",
		Deps: productruntime.HostDeps{
			Generation: 9, Components: componentLookupFake{view: view},
			Processes: processInspectorFake{identity: process, executable: "/usr/bin/opencode"},
		},
		Gateway: gatewayFake{accepted: productruntime.NativeAcceptance{NativeSessionID: "ses_one", NativeMessageID: "msg_one", AcceptedAt: acceptedAt}},
		BuildLaunch: func(context.Context, productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
			return productruntime.NativeCommand{Path: "opencode"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := driver.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	attachment := daemon.ManagedAttachment{
		ID: "attachment-one", Product: "opencode", NativeSessionID: "ses_one", State: "attached", DaemonGeneration: 9,
		Evidence: NativeEvidenceForComponent(process, "/usr/bin/opencode", "ses_one", "1"),
	}
	if _, err := adapter.Adopt(context.Background(), attachment, attachment.Evidence); err != nil {
		t.Fatal(err)
	}
	accepted, err := driver.Deliver(context.Background(), attachment, productruntime.DeliveryRequest{
		DeliveryID: "delivery-one", Mode: productruntime.DeliveryIdleWake, Body: []byte(`{"version":1}`),
	})
	if err != nil || accepted.NativeMessageID != "msg_one" {
		t.Fatalf("delivery = %#v, %v", accepted, err)
	}
	foreign := attachment
	foreign.NativeSessionID = "ses_foreign"
	if _, err := driver.Deliver(context.Background(), foreign, productruntime.DeliveryRequest{DeliveryID: "delivery-two", Mode: productruntime.DeliveryIdleWake, Body: []byte(`{}`)}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("foreign target = %v", err)
	}
}

func TestComponentPeerRenameAndReconnectUseCurrentExactBinding(t *testing.T) {
	process := procinfo.Identity{PID: 321, Start: "start", StrongStart: "strong"}
	view := productruntime.ComponentSessionView{BindingID: "binding-before", AttachmentID: "attachment-rename", NativeSessionID: "ses_rename", Generation: 9, State: "idle"}
	lookup := &reconnectingLookup{view: view}
	gateway := &renameGatewayFake{result: productruntime.NativeName{Applied: "new native title", NativeConfirmed: true}}
	driver, err := NewPeerDriver(PeerConfig{
		ProductID: "opencode", Executable: "opencode", IntegrationVersion: "1",
		Deps:    productruntime.HostDeps{Generation: 9, Components: lookup, Processes: processInspectorFake{identity: process, executable: "/usr/bin/opencode"}},
		Gateway: gateway,
		BuildLaunch: func(context.Context, productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
			return productruntime.NativeCommand{Path: "opencode"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attachment := daemon.ManagedAttachment{
		ID: "attachment-rename", Product: "opencode", NativeSessionID: "ses_rename", State: "attached", DaemonGeneration: 9,
		Evidence: NativeEvidenceForComponent(process, "/usr/bin/opencode", "ses_rename", "1"),
	}
	adapter, err := driver.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	view.BindingID = "binding-after-reconnect"
	lookup.set(view)
	if _, err := adapter.Refresh(context.Background(), attachment); err != nil {
		t.Fatalf("same-generation component reconnect = %v", err)
	}
	renamed, err := driver.Rename(context.Background(), attachment, "new native title")
	if err != nil || !renamed.NativeConfirmed || renamed.Applied != "new native title" {
		t.Fatalf("rename = %#v, %v", renamed, err)
	}
	gateway.mu.Lock()
	binding, native, name := gateway.binding, gateway.native, gateway.name
	gateway.mu.Unlock()
	if binding != "binding-after-reconnect" || native != "ses_rename" || name != "new native title" {
		t.Fatalf("rename route = %q %q %q", binding, native, name)
	}
	gateway.mu.Lock()
	gateway.result = productruntime.NativeName{Applied: "different", NativeConfirmed: true}
	gateway.mu.Unlock()
	if _, err := driver.Rename(context.Background(), attachment, "requested"); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("conflicting native rename acceptance = %v", err)
	}
	view.Generation = 10
	lookup.set(view)
	if _, err := adapter.Refresh(context.Background(), attachment); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("foreign generation reconnect = %v", err)
	}
}

func TestComponentPeerLooksUpComponentByAttachmentAndNativeSession(t *testing.T) {
	process := procinfo.Identity{PID: 321, Start: "start", StrongStart: "strong"}
	lookup := &recordingComponentLookup{view: productruntime.ComponentSessionView{
		BindingID: "binding-lookup", AttachmentID: "attachment-lookup",
		NativeSessionID: "ses_lookup", Generation: 9, State: "idle",
	}}
	driver, err := NewPeerDriver(PeerConfig{
		ProductID: "opencode", Executable: "opencode", IntegrationVersion: "1",
		Deps: productruntime.HostDeps{
			Generation: 9, Components: lookup,
			Processes: processInspectorFake{identity: process, executable: "/usr/bin/opencode"},
		},
		Gateway: gatewayFake{
			accepted: productruntime.NativeAcceptance{
				NativeSessionID: "ses_lookup", NativeMessageID: "message-lookup", AcceptedAt: time.Now(),
			},
			renamed: productruntime.NativeName{Applied: "renamed", NativeConfirmed: true},
		},
		BuildLaunch: func(context.Context, productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
			return productruntime.NativeCommand{Path: "opencode"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attachment := daemon.ManagedAttachment{
		ID: "attachment-lookup", Product: "opencode", NativeSessionID: "ses_lookup",
		State: "attached", DaemonGeneration: 9,
		Evidence: NativeEvidenceForComponent(process, "/usr/bin/opencode", "ses_lookup", "1"),
	}
	adapter, err := driver.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Refresh(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Rename(context.Background(), attachment, "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Deliver(context.Background(), attachment, productruntime.DeliveryRequest{
		DeliveryID: "delivery-lookup", Mode: productruntime.DeliveryIdleWake, Body: []byte(`{"version":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	want := []componentLookupCall{
		{attachmentID: "attachment-lookup", nativeSessionID: "ses_lookup"},
		{attachmentID: "attachment-lookup", nativeSessionID: "ses_lookup"},
		{attachmentID: "attachment-lookup", nativeSessionID: "ses_lookup"},
	}
	if len(lookup.calls) != len(want) {
		t.Fatalf("component lookup calls = %+v, want %+v", lookup.calls, want)
	}
	for index := range want {
		if lookup.calls[index] != want[index] {
			t.Fatalf("component lookup call %d = %+v, want %+v", index, lookup.calls[index], want[index])
		}
	}
}

type exactParentVerifierFake struct {
	view      productruntime.ComponentSessionView
	rejectPID int
}

func (verifier exactParentVerifierFake) VerifyComponentParent(_ context.Context, product string, attempt productruntime.ConnectorAttempt) (productruntime.ComponentSessionView, error) {
	if product == "" || attempt.ProcessIdentity.PID == verifier.rejectPID {
		return productruntime.ComponentSessionView{}, productruntime.ErrUnauthorized
	}
	return verifier.view, nil
}

func TestParentAttesterRejectsFalseModelClaimAndForeignProcess(t *testing.T) {
	view := productruntime.ComponentSessionView{BindingID: "binding-parent", AttachmentID: "attachment-parent", NativeSessionID: "ses_parent", Generation: 4}
	attester, err := NewParentAttester("opencode", exactParentVerifierFake{view: view, rejectPID: 99})
	if err != nil {
		t.Fatal(err)
	}
	attempt := productruntime.ConnectorAttempt{
		ProductID:              "opencode",
		ProcessIdentity:        procinfo.Identity{PID: 55, Start: "start", StrongStart: "strong"},
		ClaimedNativeSessionID: "ses_parent", ComponentBindingID: "binding-parent",
	}
	binding, err := attester.Attest(context.Background(), attempt)
	if err != nil || !binding.Verified || binding.AttachmentID != "attachment-parent" {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	falseClaim := attempt
	falseClaim.ClaimedNativeSessionID = "ses_forged"
	if _, err := attester.Attest(context.Background(), falseClaim); !errors.Is(err, productruntime.ErrUnauthorized) {
		t.Fatalf("false claim = %v", err)
	}
	foreignPID := attempt
	foreignPID.ProcessIdentity.PID = 99
	if _, err := attester.Attest(context.Background(), foreignPID); !errors.Is(err, productruntime.ErrUnauthorized) {
		t.Fatalf("kernel/process mismatch = %v", err)
	}
}

func TestRegisteredParentToolAssetsMatchExactAttestationContract(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate product-family test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	for _, product := range []struct {
		id      string
		asset   string
		binding string
		native  string
	}{
		{id: "opencode", asset: "opencode/agent-sessions.mjs", binding: "binding-opencode-parent", native: "ses_opencode_parent"},
		{id: "kilo", asset: "kilo/agent-sessions.mjs", binding: "binding-kilo-parent", native: "ses_kilo_parent"},
	} {
		t.Run(product.id, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(repositoryRoot, "integrations", product.asset))
			if err != nil {
				t.Fatal(err)
			}
			text := string(source)
			for _, required := range []string{
				"agent_sessions: tool({",
				"if (!boundSessionID || context.sessionID !== boundSessionID)",
				"component.callTool(callID, operation, {",
				"...argumentsValue,",
				"__agent_sessions_native_session_id: context.sessionID,",
				`component.send("tool.cancel", callID`,
				`"shell.env": async`,
				"output.env.AGENT_SESSIONS_NATIVE_SESSION_ID = sessionID",
			} {
				if !strings.Contains(text, required) {
					t.Fatalf("registered parent tool asset %q omitted exact-session contract %q", product.asset, required)
				}
			}
			callStart := strings.Index(text, "component.callTool(callID, operation, {")
			if callStart < 0 {
				t.Fatalf("registered parent tool asset %q omitted component tool relay", product.asset)
			}
			callSource := text[callStart:]
			spread := strings.Index(callSource, "...argumentsValue,")
			exactSession := strings.Index(callSource, "__agent_sessions_native_session_id: context.sessionID,")
			if spread < 0 || exactSession < 0 || spread >= exactSession {
				t.Fatalf("registered parent tool asset %q does not overwrite model-supplied identity after argument spread", product.asset)
			}

			view := productruntime.ComponentSessionView{
				BindingID: product.binding, AttachmentID: "attachment-" + product.id + "-parent",
				NativeSessionID: product.native, Generation: 4,
			}
			attester, err := NewParentAttester(product.id, exactParentVerifierFake{view: view, rejectPID: 99})
			if err != nil {
				t.Fatal(err)
			}
			attempt := productruntime.ConnectorAttempt{
				ProductID:              product.id,
				ProcessIdentity:        procinfo.Identity{PID: 55, Start: "start", StrongStart: "strong"},
				ClaimedNativeSessionID: product.native, ComponentBindingID: product.binding,
			}
			binding, err := attester.Attest(context.Background(), attempt)
			if err != nil || !binding.Verified || binding.AttachmentID != view.AttachmentID || binding.NativeSessionID != product.native {
				t.Fatalf("registered parent binding = %#v, %v", binding, err)
			}
			foreignProduct := attempt
			foreignProduct.ProductID = "foreign"
			if _, err := attester.Attest(context.Background(), foreignProduct); !errors.Is(err, productruntime.ErrUnauthorized) {
				t.Fatalf("foreign registered-tool product = %v", err)
			}
		})
	}
}
