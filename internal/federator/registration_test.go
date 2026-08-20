package federator

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestPeerRegistrationRequiresCatalogAndRestoresGroups(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	socket, listener := registrationSocket(t, root, "peer.sock")
	defer func() { _ = listener.Close() }()
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A"}, catalog: catalog,
		local: map[string]localPeer{}, remote: map[string]Peer{}, localChanged: make(chan struct{}, 1),
	}
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: "session-a", Product: "codex", Name: "worker",
		PID: os.Getpid(), ProcStart: processStart(os.Getpid()), Socket: socket,
	}
	if _, err := agent.registerPeer(registration, false); err == nil {
		t.Fatal("peer without durable preferences was registered")
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "session-a", Product: "codex", ExplicitGroups: []string{"project"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	peer, err := agent.registerPeer(registration, false)
	if err != nil {
		t.Fatal(err)
	}
	wantGroups := []string{"project", "session:host-a/session-a"}
	if !equalStrings(peer.Groups, wantGroups) {
		t.Fatalf("registered groups = %v, want %v", peer.Groups, wantGroups)
	}
	if peer.Entrypoint != "codex" || peer.PermissionMode != "default" {
		t.Fatalf("registered peer = %+v", peer)
	}
}

func TestReconcileRetiresAdapterWhenSupershimLifecycleExits(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: "claude-session", Product: "claude"}); err != nil {
		t.Fatal(err)
	}
	socket, listener := registrationSocket(t, root, "adapter.sock")
	defer func() { _ = listener.Close() }()
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
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A"}, catalog: catalog,
		local: map[string]localPeer{}, remote: map[string]Peer{}, localChanged: make(chan struct{}, 1),
	}
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: "claude-session", Product: "claude", Name: "claude",
		PID: adapter.Process.Pid, ProcStart: processStart(adapter.Process.Pid), Socket: socket,
		LifecyclePID: lifecycle.Process.Pid, LifecycleProcStart: processStart(lifecycle.Process.Pid),
	}
	if _, err := agent.registerPeer(registration, false); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = lifecycle.Wait()
	agent.reconcileRegisteredPeers()
	_ = adapter.Wait()
	if processLive(adapter.Process.Pid) || len(agent.local) != 0 {
		t.Fatalf("dead supershim retained adapter: live=%v registrations=%d", processLive(adapter.Process.Pid), len(agent.local))
	}
}

func TestPreparedClaudePeerSurvivesAgentRestartAndRetiresOnLauncherCrash(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "agent-state")
	configRoot := filepath.Join(root, "claude")
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog, err := openSessionCatalog(filepath.Join(stateDir, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-0000-0000-000000000cab"
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: sessionID, Product: "claude", ExplicitGroups: []string{"before"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
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
	socket := filepath.Join(root, strconv.Itoa(adapter.Process.Pid)+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	lifecycleRoot := ClaudePeerLifecycleRootInState(stateDir, sessionID)
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(lifecycleRoot, "launch-settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"crossSessionInbound":"accept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "claude", Name: "prepared",
		PID: adapter.Process.Pid, ProcStart: processStart(adapter.Process.Pid),
		LifecyclePID: lifecycle.Process.Pid, LifecycleProcStart: processStart(lifecycle.Process.Pid),
		LifecycleRoot: lifecycleRoot, ClaudeConfigRoot: configRoot,
	}
	registration = registrationWithClaudeKeyBaseline(t, registration)
	newAgent := func() *agent {
		return &agent{
			options: AgentOptions{HostID: "host-a", HostName: "Host A", StateDir: stateDir, ClaudeConfigDir: configRoot},
			catalog: catalog, local: map[string]localPeer{}, retirements: map[string]localPeer{},
			preparations: map[string]peerPreparation{}, preparationDir: filepath.Join(stateDir, "claude-peer-preparations"),
			localChanged: make(chan struct{}, 1),
		}
	}
	first := newAgent()
	if err := first.loadPeerPreparations(); err != nil {
		t.Fatal(err)
	}
	update := SessionPreferenceUpdate{
		SessionID: sessionID, Product: "claude", Kind: SessionKindInteractive,
		ExplicitGroups: []string{"after"}, GroupsSpecified: true, AlwaysApprove: true, AlwaysApproveSpecified: true,
	}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.preparePeerLaunch(registration, update, expected); err != nil {
		t.Fatal(err)
	}
	// A restarted agent recovers the preparation before the native adapter has
	// ever published or completed a full peer registration.
	restarted := newAgent()
	if err := restarted.loadPeerPreparations(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.preparations) != 1 {
		t.Fatalf("durable preparation count = %d", len(restarted.preparations))
	}
	if err := writeJSONAtomic(filepath.Join(configRoot, "sessions", strconv.Itoa(adapter.Process.Pid)+".json"), registryRecord{
		PID: adapter.Process.Pid, ProcStart: registration.ProcStart, SessionID: sessionID,
		MessagingSocketPath: socket,
	}); err != nil {
		t.Fatal(err)
	}
	keyName, err := ClaudeServiceKeyName(adapter.Process.Pid, socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "sessions", keyName), []byte(
		`{"peerToken":"0123456789abcdef0123456789abcdef","procStart":"`+registration.ProcStart+`"}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = lifecycle.Wait()
	restarted.reconcileRegisteredPeers()
	_ = adapter.Wait()
	restarted.reconcileRegisteredPeers()
	for _, path := range []string{
		settingsPath, socket, filepath.Join(configRoot, "sessions", strconv.Itoa(adapter.Process.Pid)+".json"),
		filepath.Join(configRoot, "sessions", keyName), restarted.preparationPath(sessionID),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("prepared-crash artifact survived: %s (%v)", path, err)
		}
	}
	if len(restarted.preparations) != 0 || processLive(adapter.Process.Pid) {
		t.Fatalf("prepared adapter survived: preparations=%d live=%v", len(restarted.preparations), processLive(adapter.Process.Pid))
	}
	restored, groups, exists, err := catalog.get(sessionID)
	if err != nil || !exists || restored.AlwaysApprove || !slices.Equal(restored.ExplicitGroups, []string{"before"}) ||
		slices.Contains(groups, "after") {
		t.Fatalf("failed prepared launch catalog rollback = %+v groups=%v exists=%v err=%v", restored, groups, exists, err)
	}
}

func TestPreparedClaudePeerCrashRemovesOnlyLaunchKeyBeforeNativeRow(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "as-prepared-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	configRoot := filepath.Join(root, "claude")
	registryDir := filepath.Join(configRoot, "sessions")
	if err := os.MkdirAll(registryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	catalog, err := openSessionCatalog(filepath.Join(stateDir, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-000000000cad"
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
	prefix := strconv.Itoa(adapter.Process.Pid) + "."
	preexistingName := prefix + strings.Repeat("b", 64) + ".key"
	managedSocket := ClaudePeerMessagingSocketPath(runtimeDir, sessionID)
	newName, err := ClaudeServiceKeyName(adapter.Process.Pid, managedSocket)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		preexistingName: `{"peerToken":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","procStart":"older"}`,
		newName:         `{"peerToken":"dddddddddddddddddddddddddddddddd","procStart":"older"}`,
	} {
		if err := os.WriteFile(filepath.Join(registryDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registration := registrationWithClaudeKeyBaseline(t, PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "claude",
		PID: adapter.Process.Pid, ProcStart: processStart(adapter.Process.Pid),
		LifecyclePID: lifecycle.Process.Pid, LifecycleProcStart: processStart(lifecycle.Process.Pid),
		LifecycleRoot: ClaudePeerLifecycleRootInState(stateDir, sessionID), ClaudeConfigRoot: configRoot,
		ClaudeSocketPath: managedSocket, ClaudeSocketPathSet: true,
	})
	agent := &agent{
		options: AgentOptions{
			HostID: "host-a", HostName: "Host A", RuntimeDir: runtimeDir,
			StateDir: stateDir, ClaudeConfigDir: configRoot,
		},
		catalog: catalog, local: map[string]localPeer{}, retirements: map[string]localPeer{},
		preparations: map[string]peerPreparation{}, preparationDir: filepath.Join(stateDir, "claude-peer-preparations"),
		localChanged: make(chan struct{}, 1),
	}
	update := SessionPreferenceUpdate{SessionID: sessionID, Product: "claude", Kind: SessionKindInteractive}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.preparePeerLaunch(registration, update, expected); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", managedSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if err := os.WriteFile(filepath.Join(registryDir, newName), []byte(
		`{"peerToken":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","procStart":"`+registration.ProcStart+`"}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = lifecycle.Wait()
	// Darwin can transiently report a just-reaped PID as unknown between the
	// kill(2) absence check and the kernel process-table lookup. Reconciliation
	// must fail closed for that observation, so retry the normal maintenance
	// pass instead of blocking on the still-live adapter after one pass.
	if !waitFor(func() bool {
		agent.reconcileRegisteredPeers()
		agent.mu.RLock()
		_, prepared := agent.preparations[sessionID]
		agent.mu.RUnlock()
		return !prepared
	}, 5*time.Second) {
		t.Fatal("prepared Claude peer was not retired after lifecycle exit")
	}
	_ = adapter.Wait()
	if _, err := os.Lstat(filepath.Join(registryDir, newName)); !os.IsNotExist(err) {
		t.Fatalf("token-only launch sidecar survived crash recovery: %v", err)
	}
	if _, err := os.Lstat(managedSocket); !os.IsNotExist(err) {
		t.Fatalf("socket-before-row artifact survived crash recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(registryDir, preexistingName)); err != nil {
		t.Fatalf("pre-release sidecar was removed: %v", err)
	}
	if len(agent.preparations) != 0 {
		t.Fatalf("token-only cleanup retained preparations: %d", len(agent.preparations))
	}
}

func TestPreparedClaudePeerRecoveryRejectsSameDisplayStartWithDifferentStrongStart(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	configRoot := filepath.Join(root, "claude")
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	current := procinfo.Read(os.Getpid())
	if current.Status != procinfo.Known || current.Start == "" || current.StrongStart == "" {
		t.Skip("strong process-start identity unavailable")
	}
	const sessionID = "00000000-0000-0000-0000-000000000cac"
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "claude",
		PID: os.Getpid(), ProcStart: current.Start, AdapterStrongStart: current.StrongStart + "-reused",
		LifecyclePID: os.Getpid(), LifecycleProcStart: current.Start,
		LifecycleStrongStart: current.StrongStart + "-reused",
		LifecycleRoot:        ClaudePeerLifecycleRootInState(stateDir, sessionID), ClaudeConfigRoot: configRoot,
	}
	agent := &agent{
		options: AgentOptions{HostID: "host-a", StateDir: stateDir, ClaudeConfigDir: configRoot},
		local:   map[string]localPeer{}, retirements: map[string]localPeer{},
		preparations:   map[string]peerPreparation{sessionID: {Registration: registration}},
		preparationDir: filepath.Join(stateDir, "claude-peer-preparations"), localChanged: make(chan struct{}, 1),
	}
	if err := os.MkdirAll(agent.preparationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(agent.preparationPath(sessionID), peerPreparation{Registration: registration}); err != nil {
		t.Fatal(err)
	}
	agent.reconcileRegisteredPeers()
	if _, ok := agent.preparations[sessionID]; !ok {
		t.Fatal("same-second recycled identity was retired")
	}
	if procinfo.Read(os.Getpid()).Status != procinfo.Known {
		t.Fatal("recovery signalled the unrelated recycled process")
	}
}

func TestLoadLegacyRawClaudePeerPreparation(t *testing.T) {
	root := t.TempDir()
	const sessionID = "00000000-0000-4000-8000-000000000208"
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "claude",
		PID: 2001, ProcStart: "adapter-start", AdapterStrongStart: "adapter-strong",
		LifecyclePID: 2002, LifecycleProcStart: "lifecycle-start", LifecycleStrongStart: "lifecycle-strong",
	}
	agent := &agent{
		preparations: map[string]peerPreparation{}, preparationDir: filepath.Join(root, "preparations"),
	}
	if err := os.MkdirAll(agent.preparationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(agent.preparationPath(sessionID), registration); err != nil {
		t.Fatal(err)
	}
	if err := agent.loadPeerPreparations(); err != nil {
		t.Fatal(err)
	}
	loaded, ok := agent.preparations[sessionID]
	if !ok || !samePreparedRegistration(loaded.Registration, registration) || loaded.RollbackPreferences {
		t.Fatalf("legacy preparation was not recovered: %+v", loaded)
	}
}

func TestPreparedClaudePeerRegisterAndCancelAreLinearizable(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	configRoot := filepath.Join(root, "claude")
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog, err := openSessionCatalog(filepath.Join(stateDir, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-000000000207"
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
	socket, listener := registrationSocket(t, root, "prepared-race.sock")
	defer func() { _ = listener.Close() }()
	lifecycleRoot := ClaudePeerLifecycleRootInState(stateDir, sessionID)
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	preparation := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "claude",
		PID: adapter.Process.Pid, ProcStart: processStart(adapter.Process.Pid),
		LifecyclePID: lifecycle.Process.Pid, LifecycleProcStart: processStart(lifecycle.Process.Pid),
		LifecycleRoot: lifecycleRoot, ClaudeConfigRoot: configRoot,
	}
	preparation = registrationWithClaudeKeyBaseline(t, preparation)
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A", StateDir: stateDir, ClaudeConfigDir: configRoot},
		catalog: catalog, local: map[string]localPeer{}, retirements: map[string]localPeer{},
		preparations: map[string]peerPreparation{}, preparationDir: filepath.Join(stateDir, "claude-peer-preparations"),
		localChanged: make(chan struct{}, 1),
	}
	update := SessionPreferenceUpdate{SessionID: sessionID, Product: "claude", Kind: SessionKindInteractive}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.preparePeerLaunch(preparation, update, expected); err != nil {
		t.Fatal(err)
	}
	registration := preparation
	registration.Name = "prepared-race"
	registration.Status = "idle"
	registration.PermissionMode = "default"
	registration.Socket = socket
	start := make(chan struct{})
	var registerErr, cancelErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, registerErr = agent.registerPeer(registration, false)
	}()
	go func() {
		defer wait.Done()
		<-start
		cancelErr = agent.cancelPeerPreparation(preparation)
	}()
	close(start)
	wait.Wait()
	preference, _, exists, err := catalog.get(sessionID)
	if err != nil || cancelErr != nil || len(agent.preparations) != 0 {
		t.Fatalf("register/cancel transaction errors: register=%v cancel=%v catalog=%v preparations=%d", registerErr, cancelErr, err, len(agent.preparations))
	}
	if registerErr == nil {
		if !exists || preference.Product != "claude" || len(agent.local) != 1 {
			t.Fatalf("registered transaction lost catalog: preference=%+v exists=%v local=%d", preference, exists, len(agent.local))
		}
	} else if exists || len(agent.local) != 0 {
		t.Fatalf("canceled transaction left partial commit: register=%v preference=%+v exists=%v local=%d", registerErr, preference, exists, len(agent.local))
	}
}

func TestCommittedClaudePeerCleanupDebtSurvivesAndRetries(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	configRoot := filepath.Join(root, "claude")
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog, err := openSessionCatalog(filepath.Join(stateDir, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-000000000209"
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
	socket, listener := registrationSocket(t, root, strconv.Itoa(adapter.Process.Pid)+".sock")
	lifecycleRoot := ClaudePeerLifecycleRootInState(stateDir, sessionID)
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	preparation := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: sessionID, Product: "claude",
		PID: adapter.Process.Pid, ProcStart: processStart(adapter.Process.Pid),
		LifecyclePID: lifecycle.Process.Pid, LifecycleProcStart: processStart(lifecycle.Process.Pid),
		LifecycleRoot: lifecycleRoot, ClaudeConfigRoot: configRoot,
	}
	preparation = registrationWithClaudeKeyBaseline(t, preparation)
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A", StateDir: stateDir, ClaudeConfigDir: configRoot},
		catalog: catalog, local: map[string]localPeer{}, retirements: map[string]localPeer{},
		preparations: map[string]peerPreparation{}, preparationDir: filepath.Join(stateDir, "claude-peer-preparations"),
		localChanged: make(chan struct{}, 1),
	}
	update := SessionPreferenceUpdate{SessionID: sessionID, Product: "claude", Kind: SessionKindInteractive}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.preparePeerLaunch(preparation, update, expected); err != nil {
		t.Fatal(err)
	}
	registration := preparation
	registration.Name, registration.Status, registration.PermissionMode, registration.Socket = "cleanup-debt", "idle", "default", socket
	if _, err := agent.registerPeer(registration, false); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(configRoot, "sessions", strconv.Itoa(adapter.Process.Pid)+".json")
	if err := writeJSONAtomic(recordPath, registryRecord{
		PID: adapter.Process.Pid, ProcStart: preparation.ProcStart, SessionID: sessionID,
		MessagingSocketPath: socket,
	}); err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("changed type"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = adapter.Process.Kill()
	_ = adapter.Wait()
	_ = lifecycle.Process.Kill()
	_ = lifecycle.Wait()
	agent.reconcileRegisteredPeers()
	if _, ok := agent.preparations[sessionID]; !ok {
		t.Fatal("failed exact cleanup discarded the durable preparation")
	}
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("failed cleanup removed its source row: %v", err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	agent.reconcileRegisteredPeers()
	if _, ok := agent.preparations[sessionID]; ok {
		t.Fatal("successful cleanup retry retained the preparation")
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("cleanup retry retained native row: %v", err)
	}
	preference, _, exists, err := catalog.get(sessionID)
	if err != nil || !exists || preference.Product != "claude" {
		t.Fatalf("committed cleanup debt rolled back catalog: preference=%+v exists=%v err=%v", preference, exists, err)
	}
}

func TestReconcileRetiresAdapterDescendantsBeforeTheyReparent(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: "claude-tree", Product: "claude"}); err != nil {
		t.Fatal(err)
	}
	socket, listener := registrationSocket(t, root, "adapter.sock")
	defer func() { _ = listener.Close() }()
	childPath := filepath.Join(root, "child.pid")
	adapter := exec.Command("sh", "-c", "sleep 30 & child=$!; printf '%s' \"$child\" > \"$1\"; wait", "sh", childPath)
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
	var childPID int
	if !waitFor(func() bool {
		body, readErr := os.ReadFile(childPath)
		childPID, _ = strconv.Atoi(strings.TrimSpace(string(body)))
		return readErr == nil && childPID > 1
	}, 2*time.Second) {
		t.Fatal("adapter did not publish descendant PID")
	}
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A"}, catalog: catalog,
		local: map[string]localPeer{}, remote: map[string]Peer{}, localChanged: make(chan struct{}, 1),
	}
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: "claude-tree", Product: "claude", Name: "claude",
		PID: adapter.Process.Pid, ProcStart: processStart(adapter.Process.Pid), Socket: socket,
		LifecyclePID: lifecycle.Process.Pid, LifecycleProcStart: processStart(lifecycle.Process.Pid),
	}
	if _, err := agent.registerPeer(registration, false); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = lifecycle.Wait()
	agent.reconcileRegisteredPeers()
	_ = adapter.Wait()
	if !waitFor(func() bool { return !processLive(childPID) }, 2*time.Second) {
		t.Fatalf("adapter descendant %d survived lifecycle retirement", childPID)
	}
}

func TestRetirementAcquiresProfileLockBeforeStoppingAdapter(t *testing.T) {
	root := t.TempDir()
	lifecycleRoot := filepath.Join(root, "lifecycle")
	configRoot := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	adapter := exec.Command("sleep", "30")
	if err := adapter.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = adapter.Process.Kill()
		_ = adapter.Wait()
	})
	pid := adapter.Process.Pid
	start := processStart(pid)
	socket := filepath.Join(root, strconv.Itoa(pid)+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	peer := localPeer{
		Peer: Peer{SessionID: "stable-session"}, PID: pid, ProcStart: start,
		Socket: socket, LifecycleRoot: lifecycleRoot, ClaudeConfigRoot: configRoot,
	}
	if err := writeJSONAtomic(filepath.Join(configRoot, "sessions", strconv.Itoa(pid)+".json"), registryRecord{
		PID: pid, SessionID: peer.SessionID, ProcStart: start, MessagingSocketPath: socket,
	}); err != nil {
		t.Fatal(err)
	}
	lockPath := ClaudePeerLifecycleLockPath(lifecycleRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(lifecycleRoot, "launch-settings.json")
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"crossSessionInbound":"accept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := retirePeerAdapter(peer); err == nil {
		t.Fatal("retirement ignored a live attachment lock")
	}
	if !processLive(pid) {
		t.Fatal("retirement stopped the adapter before acquiring the profile lock")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	retireErr := retirePeerAdapter(peer)
	_ = adapter.Wait()
	if retireErr != nil {
		// The agent is not the adapter's parent in production, so init normally
		// reaps this boundary. The fixture is the parent and must reap before
		// the exact artifact-removal retry can observe PID absence.
		if err := retirePeerAdapter(peer); err != nil {
			t.Fatal(err)
		}
	}
	if processLive(pid) {
		t.Fatal("adapter survived serialized retirement")
	}
	for _, path := range []string{socket, filepath.Join(configRoot, "sessions", strconv.Itoa(pid)+".json"), settingsPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("retirement artifact survived: %s (%v)", path, err)
		}
	}
}

func TestPublishServiceRecordRemovesOnlyAttestedStaleServiceRows(t *testing.T) {
	root := t.TempDir()
	registryDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		t.Fatal(err)
	}
	const stalePID = 1_000_000_000
	if err := writeJSONAtomic(filepath.Join(registryDir, "1000000000.json"), registryRecord{
		PID: stalePID, SessionID: "old-agent", Name: "old", Entrypoint: "agent-sessions",
		ProcStart: "stale", MessagingSocketPath: filepath.Join(root, "agent.sock"), AgentService: true,
	}); err != nil {
		t.Fatal(err)
	}
	agent := &agent{
		options:     AgentOptions{HostID: "host-a", HostName: "Host A", StateDir: root},
		registryDir: registryDir, controlPath: filepath.Join(root, "agent.sock"),
		serviceToken: strings.Repeat("a", 32),
	}
	if err := agent.publishServiceRecord(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.removeServiceRecord)
	entries, err := os.ReadDir(registryDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("service registry = %v, %v", entries, err)
	}
	rowName := strconv.Itoa(os.Getpid()) + ".json"
	keyName, keyErr := ClaudeServiceKeyName(os.Getpid(), agent.controlPath)
	if keyErr != nil || entries[0].Name() != keyName && entries[1].Name() != keyName ||
		entries[0].Name() != rowName && entries[1].Name() != rowName {
		t.Fatalf("authenticated service projection = %v, %v", entries, keyErr)
	}
	keyPath := filepath.Join(registryDir, keyName)
	keyBody, readErr := os.ReadFile(keyPath)
	keyInfo, statErr := os.Stat(keyPath)
	var key struct {
		PeerToken string `json:"peerToken"`
		ProcStart string `json:"procStart"`
	}
	if readErr != nil || statErr != nil || json.Unmarshal(keyBody, &key) != nil ||
		key.PeerToken != agent.serviceToken || key.ProcStart != processStart(os.Getpid()) {
		t.Fatalf("service key = %s, read=%v stat=%v", keyBody, readErr, statErr)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("service key mode = %v, want 0600", keyInfo.Mode())
	}
	if err := writeJSONAtomic(filepath.Join(registryDir, "1000000000.json"), registryRecord{
		PID: stalePID, AgentService: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := agent.removeStaleServiceRecords(os.Getpid()); err == nil {
		t.Fatal("malformed service marker was silently removed")
	}
}

func TestAgentControlAcceptsOnlyExactNativeServiceAuth(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A"},
		local:   map[string]localPeer{}, remote: map[string]Peer{}, remoteHosts: map[string]Host{},
		serviceToken: token,
	}
	request := func(authToken string) (Message, error) {
		server, client := net.Pipe()
		go agent.handleControl(server)
		defer func() { _ = client.Close() }()
		_ = client.SetDeadline(time.Now().Add(time.Second))
		if _, err := client.Write([]byte(`{"type":"auth","token":"` + authToken + `"}` + "\n" + `{"type":"status"}` + "\n")); err != nil {
			return Message{}, err
		}
		var response Message
		err := json.NewDecoder(client).Decode(&response)
		return response, err
	}
	response, err := request(token)
	if err != nil || response.Type != "status" || len(response.Frame) == 0 {
		t.Fatalf("authenticated native control response = %+v, %v", response, err)
	}
	if response, err = request("ffffffffffffffffffffffffffffffff"); err == nil {
		t.Fatalf("wrong native auth unexpectedly returned %+v", response)
	}
}

func registrationWithClaudeKeyBaseline(t *testing.T, registration PeerRegistration) PeerRegistration {
	t.Helper()
	baseline, err := ClaudePeerKeySidecars(registration.ClaudeConfigRoot, registration.PID)
	if err != nil {
		t.Fatal(err)
	}
	registration.ClaudeKeyBaseline = baseline
	registration.ClaudeKeyBaselineSet = true
	return registration
}

func TestPeerRegistrationPermissionAndExactUnregister(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "session-a", Product: "grok", AlwaysApprove: true, AlwaysApproveSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	socket, listener := registrationSocket(t, root, "peer.sock")
	defer func() { _ = listener.Close() }()
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A"}, catalog: catalog,
		local: map[string]localPeer{}, remote: map[string]Peer{}, localChanged: make(chan struct{}, 1),
	}
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: "session-a", Product: "grok", Name: "worker",
		PID: os.Getpid(), ProcStart: processStart(os.Getpid()), Socket: socket,
	}
	if _, err := agent.registerPeer(registration, false); err == nil {
		t.Fatal("default-mode adapter was registered for a durable yolo session")
	}
	registration.PermissionMode = "bypassPermissions"
	if _, err := agent.registerPeer(registration, false); err != nil {
		t.Fatal(err)
	}
	wrong := registration
	wrong.ProcStart = "wrong"
	if err := agent.unregisterPeer(wrong); err != nil {
		t.Fatal(err)
	}
	if len(agent.local) != 1 {
		t.Fatal("mismatched unregister removed the live adapter")
	}
	if err := agent.unregisterPeer(registration); err != nil {
		t.Fatal(err)
	}
	if len(agent.local) != 0 {
		t.Fatal("exact unregister retained the live adapter")
	}
}

func TestParentContextSeparatesAdapterAndLifecycleIdentity(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: "parent", Product: "codex"}); err != nil {
		t.Fatal(err)
	}
	socket, listener := registrationSocket(t, root, "adapter.sock")
	defer func() { _ = listener.Close() }()
	lifecycle := exec.Command("sleep", "30")
	if err := lifecycle.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lifecycle.Process.Kill(); _ = lifecycle.Wait() })
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: "parent", Product: "codex", Name: "parent",
		PID: os.Getpid(), ProcStart: processStart(os.Getpid()), Socket: socket,
		LifecyclePID: lifecycle.Process.Pid, LifecycleProcStart: processStart(lifecycle.Process.Pid),
	}
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A", RuntimeDir: filepath.Join(root, "agent-runtime")}, catalog: catalog,
		local: map[string]localPeer{}, remote: map[string]Peer{}, localChanged: make(chan struct{}, 1),
	}
	if _, err := agent.registerPeer(registration, false); err != nil {
		t.Fatal(err)
	}
	parent, err := agent.parentContext("parent")
	if err != nil {
		t.Fatal(err)
	}
	if parent.AdapterPID != os.Getpid() || parent.AdapterSocket != socket ||
		parent.PID != lifecycle.Process.Pid || parent.ProcStart != registration.LifecycleProcStart ||
		parent.AgentRuntimeDir != filepath.Join(root, "agent-runtime") {
		t.Fatalf("parent context = %+v", parent)
	}
}

func TestParentGroupDecisionRequiresLiveParentButOmittedResumeRestores(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "parent", Product: "claude", ExplicitGroups: []string{"project"}, GroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	agent := &agent{
		options: AgentOptions{HostID: "host-a", HostName: "Host A"}, catalog: catalog,
		local: map[string]localPeer{}, remote: map[string]Peer{}, localChanged: make(chan struct{}, 1),
	}
	assignment := Message{
		SessionID: "child", ParentSessionID: "parent", ParentSpecified: true,
		InheritParentGroups: true, InheritGroupsSpecified: true,
	}
	if err := agent.validatePreferenceParentUpdate(assignment); err == nil {
		t.Fatal("offline catalog parent authorized a group decision")
	}
	socket, listener := registrationSocket(t, root, "parent.sock")
	defer func() { _ = listener.Close() }()
	registration := PeerRegistration{
		Version: GroupProtocolVersion, SessionID: "parent", Product: "claude", Name: "parent",
		PID: os.Getpid(), ProcStart: processStart(os.Getpid()), Socket: socket,
	}
	if _, err := agent.registerPeer(registration, false); err != nil {
		t.Fatal(err)
	}
	if err := agent.validatePreferenceParentUpdate(assignment); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "grok", ParentSession: "parent", ParentSpecified: true,
		InheritParentGroups: true, InheritGroupsSpecified: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := agent.unregisterPeer(registration); err != nil {
		t.Fatal(err)
	}
	if err := agent.validatePreferenceParentUpdate(Message{SessionID: "child"}); err != nil {
		t.Fatalf("omitted resume did not restore after parent exit: %v", err)
	}
	if err := agent.validatePreferenceParentUpdate(Message{
		SessionID: "child", InheritParentGroups: true, InheritGroupsSpecified: true,
	}); err == nil {
		t.Fatal("explicit inheritance refresh used an offline parent")
	}
}

func registrationSocket(t *testing.T, root, name string) (string, net.Listener) {
	t.Helper()
	path := filepath.Join(root, name)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return path, listener
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
