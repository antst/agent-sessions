package dsh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type recordingComponentSender struct {
	frames chan component.Frame
}

type descendantInspector struct {
	child    procinfo.Identity
	ancestor procinfo.Identity
}

func (inspector descendantInspector) CaptureIdentity(context.Context, int) (procinfo.Identity, error) {
	return inspector.child, nil
}
func (inspector descendantInspector) ObserveIdentity(_ context.Context, identity procinfo.Identity) (procinfo.IdentityObservation, error) {
	if identity != inspector.child {
		return procinfo.IdentityObservation{Status: procinfo.IdentityStale}, nil
	}
	return procinfo.IdentityObservation{Status: procinfo.IdentityMatches, Current: identity}, nil
}
func (descendantInspector) Executable(context.Context, procinfo.Identity) (string, error) {
	return "dsh", nil
}
func (inspector descendantInspector) DescendsFrom(_ context.Context, child, ancestor procinfo.Identity, _ int) (bool, error) {
	return child == inspector.child && ancestor == inspector.ancestor, nil
}

func (sender *recordingComponentSender) Send(_ string, frameType component.FrameType, operationID string, payload any) error {
	frame, err := component.NewFrame(frameType, operationID, 1, payload)
	if err == nil {
		sender.frames <- frame
	}
	return err
}

func TestCordisGatewayBindsExactSessionAndRoutesFollowupOrSteer(t *testing.T) {
	sender := &recordingComponentSender{frames: make(chan component.Frame, 2)}
	gateway, err := NewCordisGateway(sender, func() time.Time { return time.Unix(100, 0) })
	if err != nil {
		t.Fatal(err)
	}
	binding := component.BindingView{
		BindingID: "binding", AttachmentID: "attachment", ProductID: ProductID,
		ProcessIdentity: procinfo.Identity{PID: 101, Start: "start", StrongStart: "strong"},
		PeerIdentity:    localtransport.PeerIdentity{PID: 101, UID: 1000}, Generation: 7,
	}
	announceFrame, _ := component.NewFrame(component.TypeSessionAnnounce, "announce", 1, component.SessionAnnounce{
		BindingID: "binding", NativeSessionID: "native", Cwd: "/work", NativeName: "dsh", ProductEventSeq: 1,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, announceFrame); err != nil {
		t.Fatalf("announce: %v", err)
	}

	accepted := make(chan productruntime.NativeAcceptance, 1)
	failed := make(chan error, 1)
	go func() {
		value, deliveryErr := gateway.Deliver(context.Background(), "attachment", "native", productruntime.DeliveryRequest{
			DeliveryID: "delivery", Mode: productruntime.DeliveryIdleWake, Body: []byte("hello"),
		})
		accepted <- value
		failed <- deliveryErr
	}()
	outbound := <-sender.frames
	if outbound.Type != component.TypeDeliveryPresent {
		t.Fatalf("outbound type = %s", outbound.Type)
	}
	var present component.DeliveryPresent
	if err := outbound.PayloadInto(&present); err != nil || present.Mode != string(productruntime.DeliveryIdleWake) {
		t.Fatalf("delivery payload = %+v, %v", present, err)
	}
	acceptFrame, _ := component.NewFrame(component.TypeDeliveryAccept, "delivery", 2, component.DeliveryAccept{
		DeliveryID: "delivery", NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 100,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, acceptFrame); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := <-failed; err != nil {
		t.Fatal(err)
	}
	if got := <-accepted; got.NativeSessionID != "native" || got.NativeMessageID != "message" {
		t.Fatalf("acceptance = %+v", got)
	}
}

func TestCordisGatewayLateDeliveryResultIsIdempotentAfterObserverRestart(t *testing.T) {
	gateway, err := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	binding := component.BindingView{
		BindingID: "binding", AttachmentID: "attachment", ProductID: ProductID,
		ProcessIdentity: procinfo.Identity{PID: 101, Start: "start", StrongStart: "strong"},
		PeerIdentity:    localtransport.PeerIdentity{PID: 101, UID: 1000}, Generation: 7,
	}
	announce, _ := component.NewFrame(component.TypeSessionAnnounce, "announce", 1, component.SessionAnnounce{
		BindingID: binding.BindingID, NativeSessionID: "native", Cwd: "/work", NativeName: "dsh", ProductEventSeq: 1,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, announce); err != nil {
		t.Fatal(err)
	}
	late, _ := component.NewFrame(component.TypeDeliveryAccept, "delivery", 2, component.DeliveryAccept{
		DeliveryID: "delivery", NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 100,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, late); err != nil {
		t.Fatalf("late durable acceptance observer result: %v", err)
	}
	wrong, _ := component.NewFrame(component.TypeDeliveryAccept, "wrong", 3, component.DeliveryAccept{
		DeliveryID: "wrong", NativeSessionID: "sibling", NativeMessageID: "message", AcceptedAt: 101,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, wrong); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("wrong-session late acceptance error = %v, want ErrAmbiguousSession", err)
	}
}

func TestCordisGatewayReconcilesOnlyNewerCentrallyAdmittedBinding(t *testing.T) {
	sender := &recordingComponentSender{frames: make(chan component.Frame, 1)}
	gateway, _ := NewCordisGateway(sender, time.Now)
	oldBinding := component.BindingView{
		BindingID: "binding-7", AttachmentID: "attachment", ProductID: ProductID,
		ProcessIdentity: procinfo.Identity{PID: 101, Start: "start", StrongStart: "strong"},
		PeerIdentity:    localtransport.PeerIdentity{PID: 101, UID: 1000}, Generation: 7,
	}
	announce := func(binding component.BindingView, sequence uint64) error {
		frame, err := component.NewFrame(component.TypeSessionAnnounce, "announce", sequence, component.SessionAnnounce{
			BindingID: binding.BindingID, NativeSessionID: "native", Cwd: "/work", NativeName: "dsh", ProductEventSeq: sequence,
		})
		if err != nil {
			return err
		}
		return gateway.HandleComponentFrame(context.Background(), binding, frame)
	}
	if err := announce(oldBinding, 1); err != nil {
		t.Fatal(err)
	}
	conflict := oldBinding
	conflict.BindingID = "binding-same-generation"
	if err := announce(conflict, 2); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("same-generation successor error = %v, want ErrAmbiguousSession", err)
	}
	successor := oldBinding
	successor.BindingID, successor.Generation = "binding-8", 8
	if err := announce(successor, 2); err != nil {
		t.Fatal(err)
	}
	current, ok := gateway.SessionByAttachment("attachment")
	if !ok || current.Binding.BindingID != successor.BindingID || current.NativeID != "native" {
		t.Fatalf("successor projection = %+v, %v", current, ok)
	}
	if _, ok := gateway.SessionByBinding(oldBinding.BindingID); ok {
		t.Fatal("obsolete binding projection survived newer admission")
	}
}

func TestCordisGatewayDoesNotReenterCentralForSharedFrames(t *testing.T) {
	gateway, _ := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, time.Now)
	binding := component.BindingView{
		BindingID: "binding", AttachmentID: "attachment", ProductID: ProductID,
		ProcessIdentity: procinfo.Identity{PID: 101, Start: "start", StrongStart: "strong"},
		PeerIdentity:    localtransport.PeerIdentity{PID: 101, UID: 1000}, Generation: 7,
	}
	frame, err := component.NewFrame(component.TypeToolCall, "call", 1, component.ToolCall{
		CallID: "call", Operation: "sessions.list", Arguments: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.HandleComponentFrame(context.Background(), binding, frame); err != nil {
		t.Fatalf("post-admission shared observation: %v", err)
	}
}

func TestParentAttesterRequiresBindingProcessPeerAndNativeWitness(t *testing.T) {
	sender := &recordingComponentSender{frames: make(chan component.Frame, 1)}
	gateway, _ := NewCordisGateway(sender, time.Now)
	binding := component.BindingView{
		BindingID: "binding", AttachmentID: "attachment", ProductID: ProductID,
		ProcessIdentity: procinfo.Identity{PID: 101, Start: "start", StrongStart: "strong"},
		PeerIdentity:    localtransport.PeerIdentity{PID: 101, UID: 1000}, Generation: 7,
	}
	announce, _ := component.NewFrame(component.TypeSessionAnnounce, "announce", 1, component.SessionAnnounce{
		BindingID: "binding", NativeSessionID: "native", Cwd: "/work", NativeName: "dsh", ProductEventSeq: 1,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, announce); err != nil {
		t.Fatal(err)
	}
	attester, err := NewParentAttester(gateway)
	if err != nil {
		t.Fatal(err)
	}
	attempt := productruntime.ConnectorAttempt{
		ProductID: ProductID, ComponentBindingID: "binding", ClaimedNativeSessionID: "native",
		PeerCredential: binding.PeerIdentity, ProcessIdentity: binding.ProcessIdentity,
	}
	got, err := attester.Attest(context.Background(), attempt)
	if err != nil || !got.Verified || got.AttachmentID != "attachment" || got.NativeSessionID != "native" {
		t.Fatalf("Attest() = %+v, %v", got, err)
	}
	for name, mutate := range map[string]func(*productruntime.ConnectorAttempt){
		"false native id": func(value *productruntime.ConnectorAttempt) { value.ClaimedNativeSessionID = "forged" },
		"foreign binding": func(value *productruntime.ConnectorAttempt) { value.ComponentBindingID = "other" },
		"wrong peer":      func(value *productruntime.ConnectorAttempt) { value.PeerCredential.PID++ },
		"wrong process":   func(value *productruntime.ConnectorAttempt) { value.ProcessIdentity.Start = "reused" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := attempt
			mutate(&candidate)
			if _, err := attester.Attest(context.Background(), candidate); !errors.Is(err, productruntime.ErrUnauthorized) {
				t.Fatalf("Attest() error = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestParentAttesterAcceptsExactSandboxChildOnlyWithAncestryProof(t *testing.T) {
	sender := &recordingComponentSender{frames: make(chan component.Frame, 1)}
	gateway, _ := NewCordisGateway(sender, time.Now)
	ancestor := procinfo.Identity{PID: 101, Start: "start", StrongStart: "strong"}
	binding := component.BindingView{
		BindingID: "binding", AttachmentID: "attachment", ProductID: ProductID,
		ProcessIdentity: ancestor, PeerIdentity: localtransport.PeerIdentity{PID: 101, UID: 1000}, Generation: 7,
	}
	announce, _ := component.NewFrame(component.TypeSessionAnnounce, "announce", 1, component.SessionAnnounce{
		BindingID: "binding", NativeSessionID: "native", Cwd: "/work", NativeName: "dsh", ProductEventSeq: 1,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, announce); err != nil {
		t.Fatal(err)
	}
	child := procinfo.Identity{PID: 202, Start: "child-start", StrongStart: "child-strong"}
	attester, err := NewParentAttester(gateway, descendantInspector{child: child, ancestor: ancestor})
	if err != nil {
		t.Fatal(err)
	}
	got, err := attester.Attest(context.Background(), productruntime.ConnectorAttempt{
		ProductID: ProductID, ComponentBindingID: "binding", ClaimedNativeSessionID: "native",
		PeerCredential: localtransport.PeerIdentity{PID: child.PID, UID: 1000}, ProcessIdentity: child,
	})
	if err != nil || !got.Verified || got.NativeSessionID != "native" {
		t.Fatalf("Attest(child) = %+v, %v", got, err)
	}
}

func TestPeerLaunchSocketMustBeHomeOrXDGAndNeverTmp(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	validRoot := filepath.Join(home, ".local", "state", "agent-sessions")
	dshHome := managedTestDSHHome(t)
	writeManagedProfileManifest(t, dshHome, "blue", PinnedVersion)
	writeManagedProfileManifest(t, dshHome, "green", PinnedVersion)
	gateway, err := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := NewPeerDriver(PeerConfig{Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(validRoot, "run", component.ComponentSocketName), Gateway: gateway, TupleVerifier: StaticTupleVerifier(PinnedTuple())})
	if err != nil {
		t.Fatalf("NewPeerDriver(valid): %v", err)
	}
	adapter, err := peer.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "green-attachment", Product: ProductID, ProfileIdentity: "green", Cwd: "/work"}); err != nil {
		t.Fatalf("second exact durable peer profile: %v", err)
	}
	if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "attachment", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work"}); err != nil {
		t.Fatalf("exact durable peer profile: %v", err)
	}
	request := productruntime.PeerLaunchRequest{
		ProductID: ProductID, AttachmentID: "attachment", Cwd: "/work",
		Env: []productruntime.EnvVar{{Name: "DSH_HOME", Value: filepath.Join(t.TempDir(), "foreign-home")}},
	}
	command, err := peer.BuildLaunch(context.Background(), request)
	if err != nil || command.Path != "dsh" {
		t.Fatalf("BuildLaunch() = %+v, %v", command, err)
	}
	if got := envValue(command.Env, EnvComponentVersion); got != component.ContractRevision {
		t.Fatalf("component revision = %q, want %q", got, component.ContractRevision)
	}
	if got := envValue(command.Env, "DSH_HOME"); got != dshHome {
		t.Fatalf("managed DSH_HOME = %q, want %q", got, dshHome)
	}
	nonCanonicalCwd := request
	nonCanonicalCwd.Cwd = "relative/work"
	if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "attachment", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.BuildLaunch(context.Background(), nonCanonicalCwd); !errors.Is(err, productruntime.ErrNativeRejected) {
		t.Fatalf("non-canonical launch cwd error = %v, want ErrNativeRejected", err)
	}
	if _, err := peer.BuildLaunch(context.Background(), request); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("non-canonical launch did not burn prepared profile: %v", err)
	}
	if _, err := NewPeerDriver(PeerConfig{Executable: "dsh", DSHHome: dshHome, ComponentSocket: "/tmp/agent-sessions/component.sock", TupleVerifier: StaticTupleVerifier(PinnedTuple())}); err == nil {
		t.Fatal("/tmp component socket accepted")
	}
	if _, err := NewPeerDriver(PeerConfig{Executable: "dsh", DSHHome: dshHome, ComponentSocket: "/private/tmp/agent-sessions/component.sock", AllowedRoots: []string{"/private/tmp/agent-sessions"}, TupleVerifier: StaticTupleVerifier(PinnedTuple())}); err == nil {
		t.Fatal("macOS /private/tmp component socket accepted")
	}
	symlinkRoot, err := os.MkdirTemp(home, ".agent-sessions-dsh-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(symlinkRoot) })
	if err := os.Symlink(os.TempDir(), filepath.Join(symlinkRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	symlinkPeer, err := NewPeerDriver(PeerConfig{
		Executable: "dsh", DSHHome: dshHome, AllowedRoots: []string{symlinkRoot},
		ComponentSocket: filepath.Join(symlinkRoot, "escape", component.ComponentSocketName), Gateway: gateway, TupleVerifier: StaticTupleVerifier(PinnedTuple()),
	})
	if err != nil {
		t.Fatalf("constructor performed live symlink validation: %v", err)
	}
	symlinkAdapter, err := symlinkPeer.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := symlinkAdapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "symlink", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work"}); err != nil {
		t.Fatal(err)
	}
	symlinkRequest := request
	symlinkRequest.AttachmentID = "symlink"
	if _, err := symlinkPeer.BuildLaunch(context.Background(), symlinkRequest); err == nil {
		t.Fatal("HOME path resolving through a symlink into the temporary root was accepted at operation time")
	}
	mismatch := PinnedTuple()
	mismatch.Plugin = "0.1.2-alpha.4"
	badPeer, err := NewPeerDriver(PeerConfig{Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(validRoot, "run", component.ComponentSocketName), Gateway: gateway, TupleVerifier: StaticTupleVerifier(mismatch)})
	if err != nil {
		t.Fatal(err)
	}
	badAdapter, err := badPeer.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badAdapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "attachment", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work"}); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("mismatched tuple prepare error = %v, want ErrIncompatible", err)
	}
	physicalCwd := t.TempDir()
	symlinkParent := t.TempDir()
	symlinkCwd := filepath.Join(symlinkParent, "work-link")
	if err := os.Symlink(physicalCwd, symlinkCwd); err != nil {
		t.Fatal(err)
	}
	binding := component.BindingView{
		BindingID: "cwd-binding", AttachmentID: "cwd-attachment", ProductID: ProductID,
		ProcessIdentity: procinfo.Identity{PID: 303, Start: "cwd-start", StrongStart: "cwd-strong"},
		PeerIdentity:    localtransport.PeerIdentity{PID: 303, UID: 1000}, Generation: 1,
	}
	announce, _ := component.NewFrame(component.TypeSessionAnnounce, "cwd-announce", 1, component.SessionAnnounce{
		BindingID: binding.BindingID, NativeSessionID: "cwd-native", Cwd: physicalCwd, NativeName: "dsh", ProductEventSeq: 1,
	})
	if err := gateway.HandleComponentFrame(context.Background(), binding, announce); err != nil {
		t.Fatal(err)
	}
	attachment := daemon.ManagedAttachment{ID: binding.AttachmentID, Product: ProductID, ProfileIdentity: "blue", NativeSessionID: "cwd-native", Cwd: symlinkCwd}
	if _, err := adapter.Adopt(context.Background(), attachment, daemon.NativeEvidence{Process: binding.ProcessIdentity}); err != nil {
		t.Fatalf("physically equivalent symlink cwd: %v", err)
	}
	attachment.Cwd = t.TempDir()
	if _, err := adapter.Adopt(context.Background(), attachment, daemon.NativeEvidence{Process: binding.ProcessIdentity}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("wrong durable cwd error = %v, want ErrStale", err)
	}
	writeManagedProfileManifest(t, dshHome, "blue", "0.1.2-alpha.4")
	if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "drift", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work"}); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("post-construction profile drift error = %v, want ErrIncompatible", err)
	}
}

func TestPeerRenameIsTypedUnsupportedWithoutAliasMutation(t *testing.T) {
	driver := &PeerDriver{}
	got, err := driver.Rename(context.Background(), daemon.ManagedAttachment{Product: ProductID}, "native title")
	if !errors.Is(err, productruntime.ErrUnsupportedRename) {
		t.Fatalf("Rename() error = %v, want ErrUnsupportedRename", err)
	}
	if got != (productruntime.NativeName{}) {
		t.Fatalf("Rename() result = %+v, want no alias or native-name mutation", got)
	}
}
