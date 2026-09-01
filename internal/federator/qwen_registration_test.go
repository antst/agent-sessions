package federator

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const qwenEmptyFingerprint = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestQwenPreparationEnvelopeRoundTrip(t *testing.T) {
	root := t.TempDir()
	registration := qwenTestPreparation(t, root, "00000000-0000-4000-8000-0000000000e1")
	preparation := peerPreparation{
		Registration: registration,
		DesiredPreference: SessionPreferences{
			SessionID: registration.SessionID, Product: "qwen", Kind: SessionKindInteractive,
			ExplicitGroups: []string{"project"}, Revision: "revision",
		},
		RollbackPreferences: true,
	}
	path := filepath.Join(root, "preparation.json")
	if err := writePeerPreparation(path, preparation); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePeerPreparation(body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, preparation) {
		t.Fatalf("Qwen preparation round trip\n got: %#v\nwant: %#v", decoded, preparation)
	}
}

func TestQwenLaneRegistrationUsesPublishedPermissionWithoutInteractivePreparation(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-0000000000f1"
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: sessionID, Product: "qwen", Kind: SessionKindLane,
		AlwaysApprove: true, AlwaysApproveSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	socket, listener := registrationSocket(t, root, "qwen-lane.sock")
	defer func() { _ = listener.Close() }()
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A"}, catalog: catalog,
		local: map[string]localPeer{}, remote: map[string]Peer{}, localChanged: make(chan struct{}, 1),
	}
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "qwen", Name: "qwen-lane",
		PermissionMode: "yolo", PID: os.Getpid(), ProcStart: processStart(os.Getpid()),
		Socket: socket, QwenCapabilityDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if _, err := agent.registerPeer(registration, false); err != nil {
		t.Fatal(err)
	}
}

func TestQwenPreparedLaunchRollbackRestoresCatalog(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	catalog, err := openSessionCatalog(filepath.Join(stateDir, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-0000000000e2"
	prior, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: sessionID, Product: "qwen", Kind: SessionKindInteractive,
		ExplicitGroups: []string{"before"}, GroupsSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := qwenTestAgent(t, root, stateDir, catalog)
	registration := qwenTestPreparation(t, root, sessionID)
	registration.LifecycleRoot = PeerLifecycleRootInState(stateDir, "qwen", sessionID)
	qwenRewritePreparationPaths(t, &registration)
	update := SessionPreferenceUpdate{
		SessionID: sessionID, Product: "qwen", Kind: SessionKindInteractive,
		ExplicitGroups: []string{"after"}, GroupsSpecified: true,
	}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.preparePeerLaunch(registration, update, expected); err != nil {
		t.Fatal(err)
	}
	adopted, _, ok, err := catalog.get(sessionID)
	if err != nil || !ok || !equalStrings(adopted.ExplicitGroups, []string{"after"}) {
		t.Fatalf("prepared catalog = %#v, ok=%v err=%v", adopted, ok, err)
	}
	if err := agent.cancelPeerPreparation(registration); err != nil {
		t.Fatal(err)
	}
	restored, _, ok, err := catalog.get(sessionID)
	if err != nil || !ok || !samePreferenceDecision(restored, prior) {
		t.Fatalf("restored catalog = %#v, want %#v, ok=%v err=%v", restored, prior, ok, err)
	}
	if len(agent.preparations) != 0 {
		t.Fatalf("Qwen preparation survived rollback: %#v", agent.preparations)
	}
}

func TestQwenPreparedLaunchCommitRejectsChangedProfileAndAcceptsExactPayload(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	catalog, err := openSessionCatalog(filepath.Join(stateDir, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-0000000000e3"
	agent := qwenTestAgent(t, root, stateDir, catalog)
	registration := qwenTestPreparation(t, root, sessionID)
	registration.LifecycleRoot = PeerLifecycleRootInState(stateDir, "qwen", sessionID)
	qwenRewritePreparationPaths(t, &registration)
	update := SessionPreferenceUpdate{SessionID: sessionID, Product: "qwen", Kind: SessionKindInteractive}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.preparePeerLaunch(registration, update, expected); err != nil {
		t.Fatal(err)
	}

	socket, listener := registrationSocket(t, root, "qwen.sock")
	defer func() { _ = listener.Close() }()
	changed := registration
	changed.Socket = socket
	changed.QwenPreparation = cloneQwenPreparation(registration.QwenPreparation)
	changed.QwenPreparation.Profile.Fingerprint = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, err := agent.registerPeer(changed, false); err == nil {
		t.Fatal("Qwen registration with changed profile was accepted")
	}

	exact := registration
	exact.Socket = socket
	if _, err := agent.registerPeer(exact, false); err != nil {
		t.Fatal(err)
	}
	prepared := agent.preparations[sessionID]
	if !prepared.Committed {
		t.Fatalf("Qwen preparation did not commit: %#v", prepared)
	}
}

func TestQwenPreparedCrashCleanupSurvivesAgentRestart(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	catalogPath := filepath.Join(stateDir, "sessions.json")
	catalog, err := openSessionCatalog(catalogPath, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-0000000000e4"
	agent := qwenTestAgent(t, root, stateDir, catalog)
	adapter := exec.Command("sleep", "30")
	lifecycle := exec.Command("sleep", "30")
	if err := adapter.Start(); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Start(); err != nil {
		_ = adapter.Process.Kill()
		_ = adapter.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = adapter.Process.Kill()
		_ = adapter.Wait()
		_ = lifecycle.Process.Kill()
		_ = lifecycle.Wait()
	})
	registration := qwenTestPreparation(t, root, sessionID)
	registration.LifecycleRoot = PeerLifecycleRootInState(stateDir, "qwen", sessionID)
	registration.PID, registration.ProcStart = adapter.Process.Pid, processStart(adapter.Process.Pid)
	registration.LifecyclePID, registration.LifecycleProcStart = lifecycle.Process.Pid, processStart(lifecycle.Process.Pid)
	qwenRewritePreparationPaths(t, &registration)
	update := SessionPreferenceUpdate{SessionID: sessionID, Product: "qwen", Kind: SessionKindInteractive}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.preparePeerLaunch(registration, update, expected); err != nil {
		t.Fatal(err)
	}

	reopenedCatalog, err := openSessionCatalog(catalogPath, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	restarted := qwenTestAgent(t, root, stateDir, reopenedCatalog)
	if err := restarted.loadPeerPreparations(); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = lifecycle.Wait()
	restarted.reconcileRegisteredPeers()
	deadline := time.Now().Add(2 * time.Second)
	for len(restarted.preparations) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		restarted.reconcileRegisteredPeers()
	}
	if len(restarted.preparations) != 0 {
		t.Fatalf("Qwen preparation survived restart reconciliation: %#v", restarted.preparations)
	}
	_ = adapter.Wait()
	if processLive(adapter.Process.Pid) {
		t.Fatal("Qwen adapter survived lifecycle crash reconciliation")
	}
	for _, path := range []string{registration.QwenPreparation.Input.Path, registration.QwenPreparation.Events.Path} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("Qwen crash residue survived at %s: %v", path, err)
		}
	}
	if _, _, ok, err := reopenedCatalog.get(sessionID); err != nil || ok {
		t.Fatalf("rolled-back Qwen catalog remains: ok=%v err=%v", ok, err)
	}
}

func qwenTestPreparation(t *testing.T, root, sessionID string) PeerRegistration {
	t.Helper()
	lifecycleRoot := PeerLifecycleRootInState(filepath.Join(root, "state"), "qwen", sessionID)
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "qwen", Name: "qwen-peer",
		PID: os.Getpid(), ProcStart: processStart(os.Getpid()),
		LifecyclePID: os.Getpid(), LifecycleProcStart: processStart(os.Getpid()), LifecycleRoot: lifecycleRoot,
		QwenPreparation: &QwenPreparationPayload{
			Version:      1,
			Profile:      QwenProfileIdentity{QwenHomeSet: false, Fingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			CanonicalCwd: root, LaunchPreference: "native_default", InitialModeRequest: "default",
			Input:               QwenArtifactAttestation{Path: filepath.Join(lifecycleRoot, "input.jsonl"), Fingerprint: qwenEmptyFingerprint},
			Events:              QwenArtifactAttestation{Path: filepath.Join(lifecycleRoot, "events.jsonl"), Fingerprint: qwenEmptyFingerprint},
			MCPCapabilityDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	registration.QwenCapabilityDigest = registration.QwenPreparation.MCPCapabilityDigest
	qwenRewritePreparationPaths(t, &registration)
	return registration
}

func qwenRewritePreparationPaths(t *testing.T, registration *PeerRegistration) {
	t.Helper()
	if err := os.MkdirAll(registration.LifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	registration.QwenPreparation.Input.Path = filepath.Join(registration.LifecycleRoot, "input.jsonl")
	registration.QwenPreparation.Events.Path = filepath.Join(registration.LifecycleRoot, "events.jsonl")
	for _, path := range []string{registration.QwenPreparation.Input.Path, registration.QwenPreparation.Events.Path} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	input, err := QwenArtifactAttestationForPath(registration.QwenPreparation.Input.Path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := QwenArtifactAttestationForPath(registration.QwenPreparation.Events.Path)
	if err != nil {
		t.Fatal(err)
	}
	registration.QwenPreparation.Input = input
	registration.QwenPreparation.Events = events
}

func qwenTestAgent(t *testing.T, root, stateDir string, catalog *sessionCatalog) *agent {
	t.Helper()
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A", StateDir: stateDir, RuntimeDir: filepath.Join(root, "runtime")},
		catalog: catalog, local: map[string]localPeer{}, remote: map[string]Peer{}, preparations: map[string]peerPreparation{},
		preparationDir: filepath.Join(stateDir, "peer-preparations"), localChanged: make(chan struct{}, 1),
	}
	if err := os.MkdirAll(agent.preparationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return agent
}
