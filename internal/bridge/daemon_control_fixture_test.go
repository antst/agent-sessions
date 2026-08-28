package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/testutil"
)

type bridgeDaemonTestAdapter struct{}

func (*bridgeDaemonTestAdapter) PrepareInteractive(
	_ context.Context,
	request daemonpkg.AttachmentPrepareRequest,
) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{
		Executable: "/bin/true", Cwd: request.Cwd,
		ExpectedNativeActor: cloneBridgeTestEvidence(request.ExpectedNativeActor),
	}, nil
}

func (*bridgeDaemonTestAdapter) Corroborate(
	_ context.Context,
	record daemonpkg.AttachmentRecord,
	evidence map[string]any,
) (map[string]any, error) {
	if len(evidence) == 0 {
		return cloneBridgeTestEvidence(record.NativeActor), nil
	}
	return cloneBridgeTestEvidence(evidence), nil
}

func (*bridgeDaemonTestAdapter) Reconnect(
	_ context.Context,
	record daemonpkg.AttachmentRecord,
) (map[string]any, error) {
	return cloneBridgeTestEvidence(record.NativeActor), nil
}

func (*bridgeDaemonTestAdapter) Deliver(
	ctx context.Context,
	destination daemonpkg.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	socket, _ := destination.NativeActor["socket"].(string)
	if socket == "" {
		return errors.New("test destination has no socket")
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	return json.NewEncoder(connection).Encode(frame)
}

type bridgeDaemonTestFixture struct {
	t       *testing.T
	root    string
	parents map[string]bridgeDaemonTestParent
}

type bridgeDaemonTestParent struct {
	attachmentID string
	capability   string
	product      string
	sessionID    string
}

var bridgeDaemonTestFixtures = struct {
	sync.Mutex
	byRuntime map[string]*bridgeDaemonTestFixture
}{byRuntime: make(map[string]*bridgeDaemonTestFixture)}

// useBridgeTestAgent starts the real unified daemon control/runtime boundary
// with in-process vendor adapters. It deliberately does not recreate the
// retired standalone host-agent listener or registry.
func useBridgeTestAgent(t *testing.T) string {
	t.Helper()
	root := shortSocketTestRoot(t, "bd-")
	for _, directory := range []string{"home", "config", "state", "run"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))

	adapter := &bridgeDaemonTestAdapter{}
	adapters := map[string]daemonpkg.AttachmentAdapter{}
	deliveries := map[string]daemonpkg.DeliveryAdapter{}
	for _, product := range []string{"claude", "codex", "grok", "qwen"} {
		adapters[product] = adapter
		deliveries[product] = adapter
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemonpkg.RunForegroundWithOptions(ctx, daemonpkg.ForegroundOptions{
			AttachmentAdapters: adapters,
			DeliveryAdapters:   deliveries,
		})
	}()
	if !waitForCondition(3*time.Second, func() bool {
		_, err := daemonpkg.QueryAdmin(context.Background(), "runtime.status")
		return err == nil
	}) {
		cancel()
		t.Fatalf("unified daemon test fixture did not become ready: %v", <-done)
	}
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	runtimeDir := paths.RuntimeRoot
	fixture := &bridgeDaemonTestFixture{t: t, root: root, parents: make(map[string]bridgeDaemonTestParent)}
	bridgeDaemonTestFixtures.Lock()
	bridgeDaemonTestFixtures.byRuntime[runtimeDir] = fixture
	bridgeDaemonTestFixtures.Unlock()
	t.Setenv(agentRuntimeDirEnvironment, runtimeDir)
	t.Cleanup(func() {
		bridgeDaemonTestFixtures.Lock()
		delete(bridgeDaemonTestFixtures.byRuntime, runtimeDir)
		bridgeDaemonTestFixtures.Unlock()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop unified daemon test fixture: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("unified daemon test fixture did not stop")
		}
	})
	return runtimeDir
}

func prepareBridgeTestLaneParent(t *testing.T, runtimeDir, childID, parentID string) {
	prepareBridgeTestLaneParentForProduct(t, runtimeDir, childID, parentID, "claude")
}

func prepareBridgeTestLaneParentForProduct(t *testing.T, runtimeDir, childID, parentID, product string) {
	t.Helper()
	fixture := bridgeDaemonFixture(t, runtimeDir)
	_, ok := fixture.parents[parentID]
	if !ok {
		t.Fatalf("parent %q is not attached to the unified daemon", parentID)
	}
	prepareBridgeTestStandaloneLane(t, runtimeDir, childID, product)
}

func prepareBridgeTestStandaloneLane(t *testing.T, runtimeDir, childID, product string) {
	t.Helper()
	fixture := bridgeDaemonFixture(t, runtimeDir)
	identity := fixture.attach(t, product, childID, childID, "", []string{"bridge-test"})
	t.Setenv(daemonpkg.InternalAttachmentIDEnvironment, identity.attachmentID)
	t.Setenv(daemonpkg.InternalCapabilityEnvironment, identity.capability)
	t.Setenv(daemonpkg.InternalProductEnvironment, product)
	t.Setenv(daemonpkg.InternalSessionIDEnvironment, childID)
	t.Setenv(peerSessionIDEnvironment, childID)
}

func registerBridgeTestParent(t *testing.T, runtimeDir string) (<-chan struct{}, func()) {
	t.Helper()
	fixture := bridgeDaemonFixture(t, runtimeDir)
	const parentID = "target-session"
	socket := filepath.Join(shortSocketTestRoot(t, "bp-"), "parent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 4)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_, _ = bufio.NewReader(connection).ReadBytes('\n')
			_ = connection.Close()
			received <- struct{}{}
		}
	}()
	identity := fixture.attach(t, "codex", parentID, parentID, socket, []string{"bridge-test"})
	fixture.parents[parentID] = identity
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = daemonpkg.DetachManagedAttachment(context.Background(), identity.attachmentID, "test_parent_stopped")
			_ = listener.Close()
			delete(fixture.parents, parentID)
		})
	}
	t.Cleanup(stop)
	return received, stop
}

func (fixture *bridgeDaemonTestFixture) attach(
	t *testing.T,
	product, sessionID, name, socket string,
	groups []string,
) bridgeDaemonTestParent {
	t.Helper()
	return fixture.attachProcess(t, product, sessionID, name, socket, groups, os.Getpid())
}

func (fixture *bridgeDaemonTestFixture) attachProcess(
	t *testing.T,
	product, sessionID, name, socket string,
	groups []string,
	pid int,
) bridgeDaemonTestParent {
	t.Helper()
	process := procinfo.Read(pid)
	evidence := map[string]any{
		"pid": pid, "proc_start": process.Start, "strong_start": process.StrongStart,
		"parent_pid": pid, "socket": socket,
	}
	prepared, err := daemonpkg.PrepareManagedAttachment(context.Background(), daemonpkg.AttachmentPrepareRequest{
		Product: product, Kind: federation.SessionKindInteractive, Cwd: fixture.root,
		Name: name, Groups: groups, ExpectedNativeActor: evidence,
	})
	if err != nil {
		t.Fatalf("prepare %s daemon attachment: %v", product, err)
	}
	identity := daemonpkg.LocalControlIdentity{
		Role: daemonpkg.LocalControlConnector, Product: product,
		AttachmentID: prepared.Attachment.AttachmentID, SessionID: sessionID,
		Capability: prepared.Capability, NativeActor: evidence,
	}
	if _, err := daemonpkg.QueryLocalControl(context.Background(), identity, "peer.identity", struct{}{}); err != nil {
		t.Fatalf("adopt %s daemon attachment: %v", product, err)
	}
	return bridgeDaemonTestParent{
		attachmentID: prepared.Attachment.AttachmentID, capability: prepared.Capability,
		product: product, sessionID: sessionID,
	}
}

func startLaneContextTestAgent(t *testing.T) (string, string) {
	t.Helper()
	runtimeDir := useBridgeTestAgent(t)
	return bridgeDaemonFixture(t, runtimeDir).root, runtimeDir
}

// shortSocketTestRoot avoids including the test name in Unix socket paths.
// Darwin limits sockaddr_un.sun_path to 104 bytes.
func shortSocketTestRoot(t *testing.T, pattern string) string {
	t.Helper()
	return testutil.ShortSocketRoot(t, pattern, filepath.Join("agent-sessions", "daemon.sock"))
}

func waitForCondition(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}

func qwenRequiredSchemaProperties(value any) []string {
	switch required := value.(type) {
	case []string:
		return required
	case []any:
		result := make([]string, 0, len(required))
		for _, item := range required {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func bridgeDaemonFixture(t *testing.T, runtimeDir string) *bridgeDaemonTestFixture {
	t.Helper()
	bridgeDaemonTestFixtures.Lock()
	defer bridgeDaemonTestFixtures.Unlock()
	fixture := bridgeDaemonTestFixtures.byRuntime[runtimeDir]
	if fixture == nil {
		t.Fatalf("no unified daemon fixture for runtime %q", runtimeDir)
	}
	return fixture
}

func cloneBridgeTestEvidence(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
