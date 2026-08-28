package attachmentcontrol

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/testutil"
)

type controlTestAdapter struct {
	delivered chan federation.AgentFrame
}

func (*controlTestAdapter) PrepareInteractive(
	_ context.Context,
	request daemon.AttachmentPrepareRequest,
) (daemon.NativeLaunchPlan, error) {
	return daemon.NativeLaunchPlan{
		Executable: "/bin/true", Cwd: request.Cwd,
		ExpectedNativeActor: cloneControlTestEvidence(request.ExpectedNativeActor),
	}, nil
}

func (*controlTestAdapter) Corroborate(
	_ context.Context,
	record daemon.AttachmentRecord,
	evidence map[string]any,
) (map[string]any, error) {
	if len(evidence) == 0 {
		return cloneControlTestEvidence(record.NativeActor), nil
	}
	return cloneControlTestEvidence(evidence), nil
}

func (*controlTestAdapter) Reconnect(
	_ context.Context,
	record daemon.AttachmentRecord,
) (map[string]any, error) {
	return cloneControlTestEvidence(record.NativeActor), nil
}

func (adapter *controlTestAdapter) Deliver(
	ctx context.Context,
	_ daemon.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	select {
	case adapter.delivered <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAttachmentControlDelegatesLookupContextAndRoutingToUnifiedDaemon(t *testing.T) {
	root := configureControlTestPaths(t)
	adapter := &controlTestAdapter{delivered: make(chan federation.AgentFrame, 1)}
	cancel, done := startControlTestDaemon(t, adapter)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop unified daemon: %v", err)
		}
	})

	target := attachControlTestSession(t, root, "qwen", "target-session", "reviewer", []string{"team"})
	source := attachControlTestSession(t, root, "codex", "source-session", "author", []string{"team"})

	managed, err := LookupManagedSession(filepath.Join(root, "invented-runtime"), source.sessionID)
	if err != nil || !managed.Live || managed.Preference.Product != "codex" {
		t.Fatalf("daemon-backed exact lookup = %+v, %v", managed, err)
	}
	resolved, err := ResolveSessionName(filepath.Join(root, "another-invented-runtime"), "qwen", "reviewer")
	if err != nil || resolved != target.sessionID {
		t.Fatalf("daemon-backed name lookup = %q, %v", resolved, err)
	}

	setControlTestIdentity(t, source)
	parent, err := ResolveParentContext("/not/a/control/namespace", source.sessionID)
	if err != nil || parent.SessionID != source.sessionID || parent.Product != "codex" || parent.InstanceID != source.attachmentID {
		t.Fatalf("daemon-backed parent context = %+v, %v", parent, err)
	}
	result, err := RouteAgentFrame("/also/ignored", source.sessionID, federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "control-route-1",
		Targets: []string{target.sessionID}, Content: "content-canary",
	})
	if err != nil || len(result.Deliveries) != 1 || result.Deliveries[0].Status != "accepted" {
		t.Fatalf("daemon-backed route = %+v, %v", result, err)
	}
	select {
	case frame := <-adapter.delivered:
		if frame.MessageID != "control-route-1" || frame.Content != "content-canary" {
			t.Fatalf("delivered frame = %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("unified daemon did not invoke the destination adapter")
	}

	t.Setenv(daemon.InternalCapabilityEnvironment, "wrong-capability")
	if _, err := ResolveParentContext("", source.sessionID); err == nil {
		t.Fatal("wrong attachment capability was accepted")
	}
}

func TestAttachmentControlFailsClosedWithoutDaemonPreparedIdentity(t *testing.T) {
	configureControlTestPaths(t)
	for _, name := range []string{
		daemon.InternalAttachmentIDEnvironment,
		daemon.InternalCapabilityEnvironment,
		daemon.InternalProductEnvironment,
		daemon.InternalSessionIDEnvironment,
	} {
		t.Setenv(name, "")
	}
	if _, err := RegisterPeer("/invented-runtime", PeerRegistration{
		Product: "codex", SessionID: "bare-session",
	}); err == nil {
		t.Fatal("bare peer registration created authority without daemon preparation")
	}
	if _, err := LookupManagedSession("/invented-runtime", "bare-session"); err == nil {
		t.Fatal("lookup started or synthesized a daemon")
	}
	paths, err := daemon.ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.ControlEndpoint); !os.IsNotExist(err) {
		t.Fatalf("compatibility client created a control listener: %v", err)
	}
	if _, err := os.Lstat(paths.StateRoot); !os.IsNotExist(err) {
		t.Fatalf("compatibility client created independent state: %v", err)
	}
}

type controlTestIdentity struct {
	attachmentID string
	capability   string
	product      string
	sessionID    string
}

func attachControlTestSession(
	t *testing.T,
	root, product, sessionID, name string,
	groups []string,
) controlTestIdentity {
	t.Helper()
	process := procinfo.Read(os.Getpid())
	evidence := map[string]any{
		"pid": os.Getpid(), "proc_start": process.Start, "strong_start": process.StrongStart,
	}
	prepared, err := daemon.PrepareManagedAttachment(context.Background(), daemon.AttachmentPrepareRequest{
		Product: product, Kind: federation.SessionKindInteractive, Cwd: root,
		Name: name, Groups: groups, ExpectedNativeActor: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := controlTestIdentity{
		attachmentID: prepared.Attachment.AttachmentID, capability: prepared.Capability,
		product: product, sessionID: sessionID,
	}
	if _, err := daemon.QueryLocalControl(context.Background(), daemon.LocalControlIdentity{
		Role: daemon.LocalControlConnector, Product: product, AttachmentID: identity.attachmentID,
		SessionID: sessionID, Capability: identity.capability, NativeActor: evidence,
	}, "peer.identity", struct{}{}); err != nil {
		t.Fatal(err)
	}
	return identity
}

func setControlTestIdentity(t *testing.T, identity controlTestIdentity) {
	t.Helper()
	t.Setenv(daemon.InternalAttachmentIDEnvironment, identity.attachmentID)
	t.Setenv(daemon.InternalCapabilityEnvironment, identity.capability)
	t.Setenv(daemon.InternalProductEnvironment, identity.product)
	t.Setenv(daemon.InternalSessionIDEnvironment, identity.sessionID)
}

func startControlTestDaemon(
	t *testing.T,
	adapter *controlTestAdapter,
) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.RunForegroundWithOptions(ctx, daemon.ForegroundOptions{
			AttachmentAdapters: map[string]daemon.AttachmentAdapter{"codex": adapter, "qwen": adapter},
			DeliveryAdapters:   map[string]daemon.DeliveryAdapter{"codex": adapter, "qwen": adapter},
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := daemon.QueryAdmin(context.Background(), "runtime.status"); err == nil {
			return cancel, done
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatalf("unified daemon did not become ready: %v", <-done)
	return nil, nil
}

func configureControlTestPaths(t *testing.T) string {
	t.Helper()
	root := testutil.ShortSocketRoot(t, "ac-", filepath.Join("run", "agent-sessions", "daemon.sock"))
	for _, directory := range []string{"home", "config", "state", "run"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	return root
}

func cloneControlTestEvidence(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var _ daemon.InteractiveAttachmentAdapter = (*controlTestAdapter)(nil)
var _ daemon.DeliveryAdapter = (*controlTestAdapter)(nil)
