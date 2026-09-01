package componentruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const (
	testAttachmentID = "component-attachment"
	testCapabilityID = "bootstrap-capability"
	testSecret       = "one-time-bootstrap-value"
	testRevision     = uint64(7)
)

type authorityFixture struct {
	root       string
	store      *daemon.StateStore
	identity   procinfo.Identity
	live       daemon.NativeEvidence
	peer       component.PeerEvidence
	resolver   Resolver
	claim      component.BootstrapClaim
	nextID     atomic.Int64
	resolution atomic.Int64
}

func newAuthorityFixture(t *testing.T, generation uint64, attachmentState string) *authorityFixture {
	t.Helper()
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	live := daemon.NativeEvidence{Process: identity, Executable: executable}
	root := t.TempDir()
	store, err := daemon.OpenState(root, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	attachment := daemon.ManagedAttachment{
		ID: testAttachmentID, CapabilityHash: daemon.CapabilityDigest(testSecret), Product: "codex",
		ProfileIdentity: "component-profile", CatalogRevision: testRevision, DaemonGeneration: generation,
		ComponentProtocol: component.ContractRevision, IntegrationVersion: "1.0.0",
		ExpectedEvidence: live, State: attachmentState,
	}
	if attachmentState == "attached" {
		attachment.Evidence = live
		attachment.NativeSessionID = "native-old"
	}
	catalog := daemon.Catalog{
		Host: daemon.HostRuntime{
			User: "1000", Host: "host", Generation: generation, ServiceState: "running",
			AttachmentRevision: testRevision,
		},
		Attachments: map[string]daemon.ManagedAttachment{testAttachmentID: attachment},
	}
	if _, err := store.Commit(0, catalog); err != nil {
		t.Fatal(err)
	}
	fixture := &authorityFixture{
		root: root, store: store, identity: identity, live: live,
		peer: component.PeerEvidence{
			Peer: localtransport.PeerIdentity{PID: identity.PID, UID: os.Geteuid()}, Process: identity,
		},
		claim: component.BootstrapClaim{
			ProductID: "codex", AttachmentID: testAttachmentID, BootstrapCapabilityID: testCapabilityID,
			BootstrapValue: testSecret, ProcessStart: identity.Start, StrongStart: identity.StrongStart,
			ComponentVersion: component.ContractRevision,
		},
	}
	fixture.resolver = ResolverFunc(func(
		_ context.Context,
		attachment daemon.ManagedAttachment,
		_ component.PeerEvidence,
	) (Resolution, error) {
		fixture.resolution.Add(1)
		evidence := attachment.ExpectedEvidence
		if attachment.State == "attached" {
			evidence = attachment.Evidence
		}
		return Resolution{
			BootstrapCapabilityID: testCapabilityID,
			BootstrapRevision:     testRevision,
			LiveEvidence:          evidence,
		}, nil
	})
	return fixture
}

func (f *authorityFixture) config(generation uint64) Config {
	return Config{
		Store: f.store, Generation: generation, Resolver: f.resolver,
		NewID: func() (string, error) {
			return fmt.Sprintf("binding-%d", f.nextID.Add(1)), nil
		},
	}
}

func (f *authorityFixture) authority(t *testing.T, generation uint64) *Authority {
	t.Helper()
	authority, err := New(f.config(generation))
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func TestAuthorityConcurrentBootstrapReplayAndAdoptionFence(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	authority := fixture.authority(t, 1)
	ctx := context.Background()

	const callers = 24
	results := make(chan component.Authorization, callers)
	errorsCh := make(chan error, callers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			value, err := authority.Bootstrap(ctx, fixture.claim, fixture.peer)
			results <- value
			errorsCh <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent bootstrap: %v", err)
		}
	}
	bindingID := ""
	for result := range results {
		if bindingID == "" {
			bindingID = result.BindingID
		}
		if result.BindingID != bindingID || result.BootstrapRevision != testRevision {
			t.Fatalf("bootstrap result = %+v, binding=%q", result, bindingID)
		}
	}
	snapshot, err := fixture.store.Read()
	if err != nil || len(snapshot.Catalog.ComponentBindings) != 1 {
		t.Fatalf("concurrent durable bindings = %d, %v", len(snapshot.Catalog.ComponentBindings), err)
	}
	if fixture.resolution.Load() < callers {
		t.Fatalf("fresh resolver calls = %d, want at least %d", fixture.resolution.Load(), callers)
	}

	foreign := fixture.peer
	foreign.Process.StrongStart += "-foreign"
	if _, err := authority.Bootstrap(ctx, fixture.claim, foreign); protocolCategory(err) != component.CategoryStaleProcess {
		t.Fatalf("foreign process error = %v", err)
	}
	conflictingClaim := fixture.claim
	conflictingClaim.BootstrapCapabilityID = "another-capability"
	if _, err := authority.Bootstrap(ctx, conflictingClaim, fixture.peer); protocolCategory(err) != component.CategoryUnauthorized {
		t.Fatalf("conflicting capability id error = %v", err)
	}
	conflictingClaim = fixture.claim
	conflictingClaim.BootstrapValue = "another-secret"
	if _, err := authority.Bootstrap(ctx, conflictingClaim, fixture.peer); protocolCategory(err) != component.CategoryUnauthorized {
		t.Fatalf("conflicting capability value error = %v", err)
	}

	view := bindingView(bindingID, fixture, 1, 2)
	announce := testFrame(t, component.TypeSessionAnnounce, "announce", 2, component.SessionAnnounce{
		BindingID: bindingID, NativeSessionID: "native-one", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	start = make(chan struct{})
	var adopted atomic.Bool
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _ = authority.Bootstrap(ctx, fixture.claim, fixture.peer)
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		adopted.Store(authority.HandleComponentFrame(ctx, view, announce) == nil)
	}()
	close(start)
	wait.Wait()
	if !adopted.Load() {
		t.Fatal("concurrent adoption did not commit")
	}
	if _, err := authority.Bootstrap(ctx, fixture.claim, fixture.peer); protocolCategory(err) != component.CategoryReplay {
		t.Fatalf("post-adoption bootstrap error = %v", err)
	}
	snapshot, err = fixture.store.Read()
	if err != nil || len(snapshot.Catalog.ComponentBindings) != 1 ||
		snapshot.Catalog.ComponentBindings[bindingID].State != daemon.BindingReady {
		t.Fatalf("adopted binding state = %+v, %v", snapshot.Catalog.ComponentBindings, err)
	}
}

func TestAuthorityHeartbeatAdoptsAndFencesBootstrap(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	var productAdmissions atomic.Int64
	config := fixture.config(1)
	config.Admitter = CatalogAdmitterFunc(func(
		_ context.Context,
		_ *daemon.Catalog,
		_ component.BindingView,
		_ component.Frame,
	) error {
		productAdmissions.Add(1)
		return nil
	})
	authority, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	binding, err := authority.Bootstrap(ctx, fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := authority.Bootstrap(ctx, fixture.claim, fixture.peer)
	if err != nil || replayed.BindingID != binding.BindingID {
		t.Fatalf("lost Ready bootstrap replay = %+v, %v", replayed, err)
	}

	view := bindingView(binding.BindingID, fixture, 1, 2)
	view.LastOutboundSeq = 1
	heartbeat := testFrame(t, component.TypeHeartbeat, "heartbeat", 2, component.Heartbeat{
		BindingID: binding.BindingID, LastReceivedSeq: 1,
	})
	if err := authority.HandleComponentFrame(ctx, view, heartbeat); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	durable := snapshot.Catalog.ComponentBindings[binding.BindingID]
	if durable.State != daemon.BindingReady || durable.LastInboundSeq != 2 || len(snapshot.Catalog.ComponentSessions) != 0 {
		t.Fatalf("heartbeat adoption = binding=%+v sessions=%+v", durable, snapshot.Catalog.ComponentSessions)
	}
	if productAdmissions.Load() != 0 {
		t.Fatalf("heartbeat reached product admitter %d times", productAdmissions.Load())
	}
	if _, err := authority.Bootstrap(ctx, fixture.claim, fixture.peer); protocolCategory(err) != component.CategoryReplay {
		t.Fatalf("heartbeat did not fence bootstrap: %v", err)
	}
}

func TestAuthorityRejectsContractRevisionMismatch(t *testing.T) {
	t.Run("claim component version", func(t *testing.T) {
		fixture := newAuthorityFixture(t, 1, "prepared")
		claim := fixture.claim
		claim.ComponentVersion = "1.0.0"
		if _, err := fixture.authority(t, 1).Bootstrap(context.Background(), claim, fixture.peer); protocolCategory(err) != component.CategoryUnauthorized {
			t.Fatalf("component version mismatch = %v", err)
		}
	})

	t.Run("durable component protocol", func(t *testing.T) {
		fixture := newAuthorityFixture(t, 1, "prepared")
		snapshot, err := fixture.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		catalog := snapshot.Catalog
		attachment := catalog.Attachments[testAttachmentID]
		attachment.ComponentProtocol = fmt.Sprint(component.ProtocolVersion)
		catalog.Attachments[testAttachmentID] = attachment
		if _, err := fixture.store.Commit(snapshot.Revision, catalog); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.authority(t, 1).Bootstrap(context.Background(), fixture.claim, fixture.peer); protocolCategory(err) != component.CategoryUnauthorized {
			t.Fatalf("durable component protocol mismatch = %v", err)
		}
	})
}

func TestAuthorityBootstrapOptionalProcessCorroboration(t *testing.T) {
	for _, test := range []struct {
		name        string
		process     string
		strong      string
		foreignLive bool
		want        component.Category
	}{
		{name: "omitted"},
		{name: "matching", process: "matching", strong: "matching"},
		{name: "process-start only", process: "matching", want: component.CategoryInvalidFrame},
		{name: "strong-start only", strong: "matching", want: component.CategoryInvalidFrame},
		{name: "mismatch", process: "mismatch", strong: "matching", want: component.CategoryStaleProcess},
		{name: "foreign live evidence with omitted claims", foreignLive: true, want: component.CategoryStaleProcess},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthorityFixture(t, 1, "prepared")
			claim := fixture.claim
			claim.ProcessStart = ""
			claim.StrongStart = ""
			if test.process == "matching" {
				claim.ProcessStart = fixture.identity.Start
			} else if test.process != "" {
				claim.ProcessStart = test.process
			}
			if test.strong == "matching" {
				claim.StrongStart = fixture.identity.StrongStart
			} else if test.strong != "" {
				claim.StrongStart = test.strong
			}
			peer := fixture.peer
			if test.foreignLive {
				peer.Process.StrongStart += "-foreign"
			}
			binding, err := fixture.authority(t, 1).Bootstrap(context.Background(), claim, peer)
			if got := protocolCategory(err); got != test.want {
				t.Fatalf("bootstrap category = %q, want %q: %v", got, test.want, err)
			}
			if test.want == "" && binding.BindingID == "" {
				t.Fatal("accepted bootstrap omitted durable binding")
			}
		})
	}
}

func TestAuthorityReconnectOptionalProcessCorroboration(t *testing.T) {
	for _, test := range []struct {
		name        string
		process     string
		strong      string
		foreignLive bool
		want        component.Category
	}{
		{name: "omitted"},
		{name: "matching", process: "matching", strong: "matching"},
		{name: "process-start only", process: "matching", want: component.CategoryInvalidFrame},
		{name: "strong-start only", strong: "matching", want: component.CategoryInvalidFrame},
		{name: "mismatch", process: "matching", strong: "mismatch", want: component.CategoryStaleProcess},
		{name: "foreign live evidence with omitted claims", foreignLive: true, want: component.CategoryStaleProcess},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthorityFixture(t, 1, "prepared")
			authority := fixture.authority(t, 1)
			prior, err := authority.Bootstrap(context.Background(), fixture.claim, fixture.peer)
			if err != nil {
				t.Fatal(err)
			}
			view := bindingView(prior.BindingID, fixture, 1, 2)
			view.LastOutboundSeq = 1
			heartbeat := testFrame(t, component.TypeHeartbeat, "heartbeat", 2, component.Heartbeat{
				BindingID: prior.BindingID, LastReceivedSeq: 1,
			})
			if err := authority.HandleComponentFrame(context.Background(), view, heartbeat); err != nil {
				t.Fatal(err)
			}
			claim := component.ReconnectClaim{
				AttachmentID: testAttachmentID, PriorBindingID: prior.BindingID, PriorGeneration: 1, LastReceivedSeq: 1,
			}
			if test.process == "matching" {
				claim.ProcessStart = fixture.identity.Start
			} else if test.process != "" {
				claim.ProcessStart = test.process
			}
			if test.strong == "matching" {
				claim.StrongStart = fixture.identity.StrongStart
			} else if test.strong != "" {
				claim.StrongStart = test.strong
			}
			peer := fixture.peer
			if test.foreignLive {
				peer.Process.Start += "-foreign"
			}
			successor, err := authority.Reconnect(context.Background(), claim, peer)
			if got := protocolCategory(err); got != test.want {
				t.Fatalf("reconnect category = %q, want %q: %v", got, test.want, err)
			}
			if test.want == "" && (successor.BindingID == "" || successor.BindingID == prior.BindingID) {
				t.Fatalf("accepted reconnect successor = %+v prior=%+v", successor, prior)
			}
		})
	}
}

func TestAuthorityCrossGenerationReadyLossCreatesOneBoundedSuccessor(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	first := fixture.authority(t, 1)
	prior, err := first.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}

	runtime, err := daemon.StartRuntime(context.Background(), daemon.RuntimeConfig{StateRoot: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if runtime.Generation() != 2 {
		t.Fatalf("successor runtime generation = %d", runtime.Generation())
	}

	crash := errors.New("crash after adoption commit")
	config := fixture.config(2)
	config.afterAdoptionCommit = func() error { return crash }
	second, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	foreign := fixture.peer
	foreign.Process.StrongStart += "-foreign"
	if _, err := second.Bootstrap(context.Background(), fixture.claim, foreign); protocolCategory(err) != component.CategoryStaleProcess {
		t.Fatalf("cross-generation foreign bootstrap = %v", err)
	}
	successor, err := second.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	if successor.BindingID == prior.BindingID || successor.BootstrapRevision != prior.BootstrapRevision {
		t.Fatalf("cross-generation successor = %+v, prior=%+v", successor, prior)
	}
	replayed, err := second.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil || replayed.BindingID != successor.BindingID {
		t.Fatalf("same-generation Ready-loss replay = %+v, %v", replayed, err)
	}
	snapshot, _ := fixture.store.Read()
	if len(snapshot.Catalog.ComponentBindings) != 2 ||
		snapshot.Catalog.ComponentBindings[prior.BindingID].State != daemon.BindingClosed {
		t.Fatalf("transient predecessor set = %+v", snapshot.Catalog.ComponentBindings)
	}

	announce := testFrame(t, component.TypeSessionAnnounce, "announce", 2, component.SessionAnnounce{
		BindingID: successor.BindingID, NativeSessionID: "native-one", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	err = second.HandleComponentFrame(context.Background(), bindingView(successor.BindingID, fixture, 2, 2), announce)
	if !errors.Is(err, crash) {
		t.Fatalf("adoption crash = %v", err)
	}
	snapshot, _ = fixture.store.Read()
	if snapshot.Catalog.ComponentBindings[successor.BindingID].State != daemon.BindingReady ||
		snapshot.Catalog.ComponentBindings[prior.BindingID].State != daemon.BindingClosed {
		t.Fatalf("post-crash adoption state = %+v", snapshot.Catalog.ComponentBindings)
	}
	if _, err := New(fixture.config(2)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = fixture.store.Read()
	if len(snapshot.Catalog.ComponentBindings) != 1 || snapshot.Catalog.ComponentBindings[successor.BindingID].State != daemon.BindingReady {
		t.Fatalf("bounded converged bindings = %+v", snapshot.Catalog.ComponentBindings)
	}
}

func TestAuthoritySameGenerationReconnectCreatesOnlyImmediateSuccessor(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	authority := fixture.authority(t, 1)
	prior, err := authority.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	announce := testFrame(t, component.TypeSessionAnnounce, "announce", 2, component.SessionAnnounce{
		BindingID: prior.BindingID, NativeSessionID: "native-one", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	if err := authority.HandleComponentFrame(context.Background(), bindingView(prior.BindingID, fixture, 1, 2), announce); err != nil {
		t.Fatal(err)
	}
	reconnect := component.ReconnectClaim{
		AttachmentID: testAttachmentID, PriorBindingID: prior.BindingID, PriorGeneration: 1,
		ProcessStart: fixture.identity.Start, StrongStart: fixture.identity.StrongStart, LastReceivedSeq: 1,
	}
	successor, err := authority.Reconnect(context.Background(), reconnect, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	if successor.BindingID == prior.BindingID || successor.BootstrapRevision != prior.BootstrapRevision {
		t.Fatalf("same-generation successor = %+v prior=%+v", successor, prior)
	}
	replay, err := authority.Reconnect(context.Background(), reconnect, fixture.peer)
	if err != nil || replay.BindingID != successor.BindingID {
		t.Fatalf("same-generation reconnect replay = %+v, %v", replay, err)
	}
	snapshot, _ := fixture.store.Read()
	live := 0
	for _, binding := range snapshot.Catalog.ComponentBindings {
		if binding.State == daemon.BindingBinding || binding.State == daemon.BindingReady {
			live++
		}
	}
	if live != 1 || len(snapshot.Catalog.ComponentBindings) != 2 {
		t.Fatalf("same-generation successor set = %+v", snapshot.Catalog.ComponentBindings)
	}
	foreign := fixture.peer
	foreign.Process.Start += "-foreign"
	if _, err := authority.Reconnect(context.Background(), reconnect, foreign); protocolCategory(err) != component.CategoryStaleProcess {
		t.Fatalf("foreign reconnect error = %v", err)
	}
}

func TestAuthorityReconnectClosedSessionDeleteCrashAndExactReannounce(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	first := fixture.authority(t, 1)
	prior, err := first.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	announceOne := testFrame(t, component.TypeSessionAnnounce, "announce-one", 2, component.SessionAnnounce{
		BindingID: prior.BindingID, NativeSessionID: "native-one", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	if err := first.HandleComponentFrame(context.Background(), bindingView(prior.BindingID, fixture, 1, 2), announceOne); err != nil {
		t.Fatal(err)
	}

	runtime, err := daemon.StartRuntime(context.Background(), daemon.RuntimeConfig{StateRoot: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	deleteCrash := errors.New("crash after closed session delete")
	config := fixture.config(2)
	config.afterClosedSessionDelete = func() error { return deleteCrash }
	second, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Bootstrap(context.Background(), fixture.claim, fixture.peer); protocolCategory(err) != component.CategoryReplay {
		t.Fatalf("adopted bootstrap replay after restart = %v", err)
	}
	reconnect := component.ReconnectClaim{
		AttachmentID: testAttachmentID, PriorBindingID: prior.BindingID, PriorGeneration: 1,
		ProcessStart: fixture.identity.Start, StrongStart: fixture.identity.StrongStart, LastReceivedSeq: 1,
	}
	successor, err := second.Reconnect(context.Background(), reconnect, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := second.Reconnect(context.Background(), reconnect, fixture.peer)
	if err != nil || replayed.BindingID != successor.BindingID {
		t.Fatalf("reconnect Ready-loss replay = %+v, %v", replayed, err)
	}

	stateFrame := testFrame(t, component.TypeSessionState, "state-before-announce", 2, component.SessionState{
		NativeSessionID: "native-one", State: "idle", ProductEventSeq: 2,
	})
	view := bindingView(successor.BindingID, fixture, 2, 2)
	if err := second.HandleComponentFrame(context.Background(), view, stateFrame); protocolCategory(err) != component.CategoryUnauthorized {
		t.Fatalf("pre-announce session operation error = %v", err)
	}
	announceTwo := testFrame(t, component.TypeSessionAnnounce, "announce-two", 2, component.SessionAnnounce{
		BindingID: successor.BindingID, NativeSessionID: "native-one", Cwd: "/work", NativeName: "native", ProductEventSeq: 2,
	})
	if err := second.HandleComponentFrame(context.Background(), view, announceTwo); !errors.Is(err, deleteCrash) {
		t.Fatalf("closed-session delete crash = %v", err)
	}
	snapshot, _ := fixture.store.Read()
	if _, exists := snapshot.Catalog.ComponentSessions[testAttachmentID]; exists {
		t.Fatalf("closed session survived delete commit: %+v", snapshot.Catalog.ComponentSessions)
	}
	if err := second.HandleComponentFrame(context.Background(), view, announceTwo); err != nil {
		t.Fatal(err)
	}
	if err := second.HandleComponentFrame(context.Background(), view, announceTwo); err != nil {
		t.Fatalf("duplicate exact reannounce: %v", err)
	}
	snapshot, _ = fixture.store.Read()
	session := snapshot.Catalog.ComponentSessions[testAttachmentID]
	if session.State != daemon.ComponentSessionAnnounced || session.BindingID != successor.BindingID ||
		len(snapshot.Catalog.ComponentBindings) != 1 {
		t.Fatalf("reannounced durable authority = session=%+v bindings=%+v", session, snapshot.Catalog.ComponentBindings)
	}
	if _, err := second.Reconnect(context.Background(), reconnect, fixture.peer); protocolCategory(err) != component.CategoryReplay {
		t.Fatalf("adoption did not fence predecessor: %v", err)
	}
}

func TestAuthorityRejectsParentToolFramesBeforeSessionAnnouncement(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	var admitted atomic.Int64
	config := fixture.config(1)
	config.Admitter = CatalogAdmitterFunc(func(
		_ context.Context,
		_ *daemon.Catalog,
		_ component.BindingView,
		frame component.Frame,
	) error {
		if frame.Type == component.TypeToolCall || frame.Type == component.TypeToolCancel {
			admitted.Add(1)
		}
		return nil
	})
	authority, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := authority.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	view := bindingView(binding.BindingID, fixture, 1, 2)
	privileged := []component.Frame{
		testFrame(t, component.TypeToolCall, "tool-call", 2, component.ToolCall{
			CallID: "call-one", Operation: "parent-operation", Arguments: []byte(`{"value":1}`),
		}),
		testFrame(t, component.TypeToolCancel, "tool-cancel", 2, component.ToolCancel{CallID: "call-one"}),
		testFrame(t, component.TypeDeliveryReject, "delivery-reject", 2, component.DeliveryReject{
			DeliveryID: "delivery-one", Category: component.Category("native-refused"), Detail: "refused",
		}),
	}
	for _, frame := range privileged {
		if err := authority.HandleComponentFrame(context.Background(), view, frame); protocolCategory(err) != component.CategoryUnauthorized {
			t.Fatalf("pre-announce %s error = %v", frame.Type, err)
		}
	}
	if admitted.Load() != 0 {
		t.Fatalf("pre-announce privileged admissions = %d", admitted.Load())
	}
	snapshot, _ := fixture.store.Read()
	if snapshot.Catalog.ComponentBindings[binding.BindingID].State != daemon.BindingBinding {
		t.Fatalf("pre-announce tool frame adopted binding: %+v", snapshot.Catalog.ComponentBindings[binding.BindingID])
	}

	announce := testFrame(t, component.TypeSessionAnnounce, "announce", 2, component.SessionAnnounce{
		BindingID: binding.BindingID, NativeSessionID: "native-one", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	if err := authority.HandleComponentFrame(context.Background(), view, announce); err != nil {
		t.Fatal(err)
	}
	view.LastInboundSeq = 3
	toolAfterAnnounce := testFrame(t, component.TypeToolCall, "tool-call-after", 3, component.ToolCall{
		CallID: "call-two", Operation: "parent-operation", Arguments: []byte(`{"value":2}`),
	})
	if err := authority.HandleComponentFrame(context.Background(), view, toolAfterAnnounce); err != nil {
		t.Fatal(err)
	}
	if admitted.Load() != 1 {
		t.Fatalf("post-announce tool admissions = %d", admitted.Load())
	}
}

func TestAuthoritySessionRebindConvergesAcrossEveryCommit(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	baseConfig := fixture.config(1)
	baseConfig.Rebinder = SessionRebinderFunc(func(
		_ context.Context,
		attachment daemon.ManagedAttachment,
		oldID string,
		newID string,
		_ []byte,
	) (daemon.NativeEvidence, error) {
		if attachment.NativeSessionID != oldID || newID != "native-new" {
			return daemon.NativeEvidence{}, errors.New("rebind mismatch")
		}
		return fixture.live, nil
	})
	authority, err := New(baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := authority.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	attachFixture(t, fixture, "native-old")
	announce := testFrame(t, component.TypeSessionAnnounce, "announce", 2, component.SessionAnnounce{
		BindingID: binding.BindingID, NativeSessionID: "native-old", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	view := bindingView(binding.BindingID, fixture, 1, 2)
	if err := authority.HandleComponentFrame(context.Background(), view, announce); err != nil {
		t.Fatal(err)
	}
	rebind := testFrame(t, component.TypeSessionRebind, "rebind", 3, component.SessionRebind{
		BindingID: binding.BindingID, OldNativeSessionID: "native-old", NewNativeSessionID: "native-new",
		Evidence: []byte(`{"proof":"fresh"}`), ProductEventSeq: 2,
	})
	view.LastInboundSeq = 3

	closeCrash := errors.New("crash after rebind close")
	closeConfig := baseConfig
	closeConfig.afterRebindClose = func() error { return closeCrash }
	closeAuthority, err := New(closeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeAuthority.HandleComponentFrame(context.Background(), view, rebind); !errors.Is(err, closeCrash) {
		t.Fatalf("rebind close crash = %v", err)
	}
	snapshot, _ := fixture.store.Read()
	if snapshot.Catalog.ComponentSessions[testAttachmentID].State != daemon.ComponentSessionClosed ||
		snapshot.Catalog.Attachments[testAttachmentID].NativeSessionID != "native-old" {
		t.Fatalf("rebind CAS1 = session=%+v attachment=%+v", snapshot.Catalog.ComponentSessions[testAttachmentID], snapshot.Catalog.Attachments[testAttachmentID])
	}

	attachmentCrash := errors.New("crash after rebind attachment")
	attachmentConfig := baseConfig
	attachmentConfig.afterRebindAttachment = func() error { return attachmentCrash }
	attachmentAuthority, err := New(attachmentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachmentAuthority.HandleComponentFrame(context.Background(), view, rebind); !errors.Is(err, attachmentCrash) {
		t.Fatalf("rebind attachment crash = %v", err)
	}
	snapshot, _ = fixture.store.Read()
	if _, exists := snapshot.Catalog.ComponentSessions[testAttachmentID]; exists ||
		snapshot.Catalog.Attachments[testAttachmentID].NativeSessionID != "native-new" {
		t.Fatalf("rebind CAS2 = sessions=%+v attachment=%+v", snapshot.Catalog.ComponentSessions, snapshot.Catalog.Attachments[testAttachmentID])
	}

	finalAuthority, err := New(baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalAuthority.HandleComponentFrame(context.Background(), view, rebind); err != nil {
		t.Fatal(err)
	}
	if err := finalAuthority.HandleComponentFrame(context.Background(), view, rebind); err != nil {
		t.Fatalf("duplicate exact session rebind: %v", err)
	}
	conflict := testFrame(t, component.TypeSessionRebind, "rebind-conflict", 4, component.SessionRebind{
		BindingID: binding.BindingID, OldNativeSessionID: "native-other", NewNativeSessionID: "native-third",
		Evidence: []byte(`{"proof":"conflict"}`), ProductEventSeq: 3,
	})
	view.LastInboundSeq = 4
	if err := finalAuthority.HandleComponentFrame(context.Background(), view, conflict); protocolCategory(err) != component.CategoryReplay {
		t.Fatalf("conflicting session rebind error = %v", err)
	}
	snapshot, _ = fixture.store.Read()
	session := snapshot.Catalog.ComponentSessions[testAttachmentID]
	if session.NativeSessionID != "native-new" || session.State != daemon.ComponentSessionAnnounced || session.LastEventSeq != 2 {
		t.Fatalf("rebind CAS3 = %+v", session)
	}
}

func TestAuthorityFirstReconnectFrameRebindAdoptsAndCollectsPredecessor(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	baseConfig := fixture.config(1)
	baseConfig.Rebinder = SessionRebinderFunc(func(
		_ context.Context,
		attachment daemon.ManagedAttachment,
		oldID string,
		newID string,
		_ []byte,
	) (daemon.NativeEvidence, error) {
		if attachment.NativeSessionID != oldID || oldID != "native-old" || newID != "native-new" {
			return daemon.NativeEvidence{}, errors.New("rebind mismatch")
		}
		return fixture.live, nil
	})
	authority, err := New(baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := authority.Bootstrap(context.Background(), fixture.claim, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	attachFixture(t, fixture, "native-old")
	announce := testFrame(t, component.TypeSessionAnnounce, "announce", 2, component.SessionAnnounce{
		BindingID: prior.BindingID, NativeSessionID: "native-old", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	if err := authority.HandleComponentFrame(context.Background(), bindingView(prior.BindingID, fixture, 1, 2), announce); err != nil {
		t.Fatal(err)
	}
	reconnect := component.ReconnectClaim{
		AttachmentID: testAttachmentID, PriorBindingID: prior.BindingID, PriorGeneration: 1,
		ProcessStart: fixture.identity.Start, StrongStart: fixture.identity.StrongStart, LastReceivedSeq: 1,
	}
	successor, err := authority.Reconnect(context.Background(), reconnect, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}

	crash := errors.New("crash after rebind adoption")
	crashConfig := baseConfig
	crashConfig.afterAdoptionCommit = func() error { return crash }
	crashing, err := New(crashConfig)
	if err != nil {
		t.Fatal(err)
	}
	rebind := testFrame(t, component.TypeSessionRebind, "rebind", 2, component.SessionRebind{
		BindingID: successor.BindingID, OldNativeSessionID: "native-old", NewNativeSessionID: "native-new",
		Evidence: []byte(`{"proof":"fresh"}`), ProductEventSeq: 2,
	})
	if err := crashing.HandleComponentFrame(context.Background(), bindingView(successor.BindingID, fixture, 1, 2), rebind); !errors.Is(err, crash) {
		t.Fatalf("rebind adoption crash = %v", err)
	}
	snapshot, _ := fixture.store.Read()
	if snapshot.Catalog.ComponentBindings[successor.BindingID].State != daemon.BindingReady ||
		snapshot.Catalog.ComponentBindings[prior.BindingID].State != daemon.BindingClosed {
		t.Fatalf("post-CAS3 binding set = %+v", snapshot.Catalog.ComponentBindings)
	}

	converged, err := New(baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := converged.HandleComponentFrame(context.Background(), bindingView(successor.BindingID, fixture, 1, 2), rebind); err != nil {
		t.Fatalf("duplicate exact rebind convergence: %v", err)
	}
	snapshot, _ = fixture.store.Read()
	session := snapshot.Catalog.ComponentSessions[testAttachmentID]
	if len(snapshot.Catalog.ComponentBindings) != 1 || snapshot.Catalog.ComponentBindings[successor.BindingID].State != daemon.BindingReady ||
		session.BindingID != successor.BindingID || session.NativeSessionID != "native-new" || session.State != daemon.ComponentSessionAnnounced {
		t.Fatalf("converged first-frame rebind = bindings=%+v session=%+v", snapshot.Catalog.ComponentBindings, session)
	}
}

func TestAuthorityRealBrokerHeartbeatFencesRestartBootstrapAndPermitsReconnect(t *testing.T) {
	fixture := newAuthorityFixture(t, 1, "prepared")
	firstAuthority := fixture.authority(t, 1)
	broker, err := component.NewBroker(component.Config{
		Generation: 1, Authorizer: firstAuthority, Handler: firstAuthority, HeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	componentPath := filepath.Join(fixture.root, component.ComponentSocketName)
	stop := serveBroker(t, broker, componentPath)
	connection := dialComponent(t, componentPath)
	claim := fixture.claim
	claim.ProcessStart = ""
	claim.StrongStart = ""
	writeComponentFrame(t, connection, component.TypeBootstrap, "bootstrap", 1, claim)
	ready := readReady(t, connection)
	writeComponentFrame(t, connection, component.TypeHeartbeat, "heartbeat", 2, component.Heartbeat{
		BindingID: ready.BindingID, LastReceivedSeq: 1,
	})
	body, err := connection.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	ack, err := component.DecodeFrame(body)
	if err != nil || ack.Type != component.TypeHeartbeatAck {
		t.Fatalf("heartbeat response = %+v, %v", ack, err)
	}
	snapshot, err := fixture.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if durable := snapshot.Catalog.ComponentBindings[ready.BindingID]; durable.State != daemon.BindingReady || durable.LastInboundSeq != 2 {
		t.Fatalf("heartbeat ack preceded durable adoption: %+v", durable)
	}
	if _, err := firstAuthority.Bootstrap(context.Background(), claim, fixture.peer); protocolCategory(err) != component.CategoryReplay {
		t.Fatalf("post-heartbeat bootstrap = %v", err)
	}
	_ = connection.Close()
	stop()

	runtime, err := daemon.StartRuntime(context.Background(), daemon.RuntimeConfig{StateRoot: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	secondAuthority := fixture.authority(t, runtime.Generation())
	if _, err := secondAuthority.Bootstrap(context.Background(), claim, fixture.peer); protocolCategory(err) != component.CategoryReplay {
		t.Fatalf("restart bootstrap after heartbeat = %v", err)
	}
	reconnect := component.ReconnectClaim{
		AttachmentID: testAttachmentID, PriorBindingID: ready.BindingID, PriorGeneration: 1,
		LastReceivedSeq: 1,
	}
	successor, err := secondAuthority.Reconnect(context.Background(), reconnect, fixture.peer)
	if err != nil {
		t.Fatal(err)
	}
	if successor.BindingID == ready.BindingID || successor.BootstrapRevision != testRevision {
		t.Fatalf("restart reconnect successor = %+v prior=%+v", successor, ready)
	}
}

func TestAuthorityRealBrokerReadyLossAcrossRuntimeRestart(t *testing.T) {
	identity, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	firstRuntime, err := daemon.StartRuntime(context.Background(), daemon.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	live := daemon.NativeEvidence{Process: identity, Executable: executable}
	seedAttachment(t, firstRuntime.State(), daemon.ManagedAttachment{
		ID: testAttachmentID, CapabilityHash: daemon.CapabilityDigest(testSecret), Product: "codex",
		ProfileIdentity: "profile", CatalogRevision: testRevision, DaemonGeneration: firstRuntime.Generation(),
		ExpectedEvidence: live, ComponentProtocol: component.ContractRevision, IntegrationVersion: "1.0.0", State: "prepared",
	})
	resolver := ResolverFunc(func(_ context.Context, _ daemon.ManagedAttachment, _ component.PeerEvidence) (Resolution, error) {
		return Resolution{BootstrapCapabilityID: testCapabilityID, BootstrapRevision: testRevision, LiveEvidence: live}, nil
	})
	var ids atomic.Int64
	newAuthority := func(store *daemon.StateStore, generation uint64) *Authority {
		authority, newErr := New(Config{
			Store: store, Generation: generation, Resolver: resolver,
			NewID: func() (string, error) { return fmt.Sprintf("broker-binding-%d", ids.Add(1)), nil },
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		return authority
	}
	claim := component.BootstrapClaim{
		ProductID: "codex", AttachmentID: testAttachmentID, BootstrapCapabilityID: testCapabilityID,
		BootstrapValue: testSecret, ProcessStart: identity.Start, StrongStart: identity.StrongStart,
		ComponentVersion: component.ContractRevision,
	}

	firstAuthority := newAuthority(firstRuntime.State(), firstRuntime.Generation())
	firstBroker, err := component.NewBroker(component.Config{
		Generation: firstRuntime.Generation(), Authorizer: firstAuthority, Handler: firstAuthority,
		HeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	componentPath := filepath.Join(root, component.ComponentSocketName)
	stopFirst := serveBroker(t, firstBroker, componentPath)
	connection := dialComponent(t, componentPath)
	writeComponentFrame(t, connection, component.TypeBootstrap, "bootstrap-one", 1, claim)
	readyOne := readReady(t, connection)
	_ = connection.Close()
	stopFirst()
	if err := firstRuntime.Close(); err != nil {
		t.Fatal(err)
	}

	secondRuntime, err := daemon.StartRuntime(context.Background(), daemon.RuntimeConfig{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondRuntime.Close() })
	secondAuthority := newAuthority(secondRuntime.State(), secondRuntime.Generation())
	secondBroker, err := component.NewBroker(component.Config{
		Generation: secondRuntime.Generation(), Authorizer: secondAuthority, Handler: secondAuthority,
		HeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopSecond := serveBroker(t, secondBroker, componentPath)
	t.Cleanup(stopSecond)
	restarted := dialComponent(t, componentPath)
	defer restarted.Close()
	writeComponentFrame(t, restarted, component.TypeBootstrap, "bootstrap-two", 1, claim)
	readyTwo := readReady(t, restarted)
	if readyTwo.BindingID == readyOne.BindingID || readyTwo.DaemonGeneration != secondRuntime.Generation() {
		t.Fatalf("restart Ready = first=%+v second=%+v", readyOne, readyTwo)
	}
	writeComponentFrame(t, restarted, component.TypeSessionAnnounce, "announce", 2, component.SessionAnnounce{
		BindingID: readyTwo.BindingID, NativeSessionID: "native-one", Cwd: "/work", NativeName: "native", ProductEventSeq: 1,
	})
	eventually(t, func() bool {
		snapshot, readErr := secondRuntime.State().Read()
		return readErr == nil && len(snapshot.Catalog.ComponentBindings) == 1 &&
			snapshot.Catalog.ComponentBindings[readyTwo.BindingID].State == daemon.BindingReady
	})
}

func attachFixture(t *testing.T, fixture *authorityFixture, nativeSessionID string) {
	t.Helper()
	for {
		snapshot, err := fixture.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		catalog := snapshot.Catalog
		attachment := catalog.Attachments[testAttachmentID]
		attachment.State = "attached"
		attachment.NativeSessionID = nativeSessionID
		attachment.Evidence = fixture.live
		attachment.DaemonGeneration = catalog.Host.Generation
		catalog.Attachments[testAttachmentID] = attachment
		if _, err := fixture.store.Commit(snapshot.Revision, catalog); err == nil {
			return
		}
	}
}

func seedAttachment(t *testing.T, store *daemon.StateStore, attachment daemon.ManagedAttachment) {
	t.Helper()
	for {
		snapshot, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		catalog := snapshot.Catalog
		catalog.Host.AttachmentRevision = attachment.CatalogRevision
		catalog.Attachments[attachment.ID] = attachment
		if _, err := store.Commit(snapshot.Revision, catalog); err == nil {
			return
		}
	}
}

func bindingView(bindingID string, fixture *authorityFixture, generation, inbound uint64) component.BindingView {
	return component.BindingView{
		BindingID: bindingID, AttachmentID: testAttachmentID, ProductID: "codex",
		ProcessIdentity: fixture.identity, PeerIdentity: fixture.peer.Peer,
		Generation: generation, BootstrapRevision: testRevision, LastInboundSeq: inbound,
	}
}

func testFrame(t *testing.T, frameType component.FrameType, id string, seq uint64, payload any) component.Frame {
	t.Helper()
	frame, err := component.NewFrame(frameType, id, seq, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := component.EncodeFrame(frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func protocolCategory(err error) component.Category {
	var protocolErr *component.ProtocolError
	if errors.As(err, &protocolErr) {
		return protocolErr.Category
	}
	return ""
}

func serveBroker(t *testing.T, broker *component.Broker, path string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Serve(ctx, path) }()
	eventually(t, func() bool {
		connection, err := net.DialTimeout("unix", path, 10*time.Millisecond)
		if err != nil {
			return false
		}
		_ = connection.Close()
		return true
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve broker: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("broker did not stop")
			}
		})
	}
}

func dialComponent(t *testing.T, path string) *localtransport.Conn {
	t.Helper()
	connection, err := localtransport.Dial(path, localtransport.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func writeComponentFrame(
	t *testing.T,
	connection *localtransport.Conn,
	frameType component.FrameType,
	id string,
	seq uint64,
	payload any,
) {
	t.Helper()
	frame := testFrame(t, frameType, id, seq, payload)
	body, err := component.EncodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteFrame(body); err != nil {
		t.Fatal(err)
	}
}

func readReady(t *testing.T, connection *localtransport.Conn) component.Ready {
	t.Helper()
	body, err := connection.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := component.DecodeFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != component.TypeReady {
		var rejection component.Reject
		_ = frame.PayloadInto(&rejection)
		t.Fatalf("handshake response = %s %+v", frame.Type, rejection)
	}
	var ready component.Ready
	if err := frame.PayloadInto(&ready); err != nil {
		t.Fatal(err)
	}
	return ready
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
