package federator

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	privateRoot := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(privateRoot, "sessions"), 0700); err != nil {
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
		Socket: socket, PrivateConfigRoot: privateRoot,
	}
	if err := writeJSONAtomic(filepath.Join(privateRoot, "sessions", strconv.Itoa(pid)+".json"), registryRecord{
		PID: pid, SessionID: peer.SessionID, ProcStart: start, MessagingSocketPath: socket,
	}); err != nil {
		t.Fatal(err)
	}
	lockPath := ClaudePeerLifecycleLockPath(privateRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
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
	for _, path := range []string{socket, filepath.Join(privateRoot, "sessions", strconv.Itoa(pid)+".json")} {
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
	}
	if err := agent.publishServiceRecord(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.removeServiceRecord)
	entries, err := os.ReadDir(registryDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != strconv.Itoa(os.Getpid())+".json" {
		t.Fatalf("service registry = %v, %v", entries, err)
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
		options: AgentOptions{HostID: "host-a", HostName: "Host A"}, catalog: catalog,
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
		parent.PID != lifecycle.Process.Pid || parent.ProcStart != registration.LifecycleProcStart {
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
