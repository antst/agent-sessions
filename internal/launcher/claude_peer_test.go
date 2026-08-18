package launcher

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/federator"
)

const claudePeerNativeHelperEnv = "AGENT_SESSIONS_TEST_CLAUDE_PEER_NATIVE"

func TestClaudePeerNativeHelper(_ *testing.T) {
	if os.Getenv(claudePeerNativeHelperEnv) != "1" {
		return
	}
	sessionID := ""
	permissionMode := "default"
	for index, argument := range os.Args {
		if argument == "--session-id" && index+1 < len(os.Args) {
			sessionID = os.Args[index+1]
		}
		if argument == "--resume" && index+1 < len(os.Args) {
			sessionID = os.Args[index+1]
		}
		if argument == "--dangerously-skip-permissions" {
			permissionMode = "bypassPermissions"
		}
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	socket := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), strconv.Itoa(os.Getpid())+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(71)
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
	row := map[string]any{
		"pid": os.Getpid(), "sessionId": sessionID, "cwd": config, "name": "native-test",
		"permissionMode": permissionMode, "messagingSocketPath": socket, "startedAt": time.Now().UnixMilli(),
		"procStart": federator.ProcessStart(os.Getpid()), "entrypoint": "cli", "kind": "interactive", "status": "idle",
	}
	body, _ := json.Marshal(row)
	if err := os.MkdirAll(filepath.Join(config, "sessions"), 0700); err != nil {
		os.Exit(72)
	}
	if err := os.WriteFile(filepath.Join(config, "sessions", strconv.Itoa(os.Getpid())+".json"), body, 0600); err != nil {
		os.Exit(73)
	}
	time.Sleep(700 * time.Millisecond)
	_ = listener.Close()
}

func TestParseClaudePeerArgsSeparatesGroupsAndStableSession(t *testing.T) {
	plan, err := parseClaudePeerArgs([]string{
		"--group", "project", "--group=review", "--peer-name", "worker", "--dangerously-skip-permissions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !threadIDPattern.MatchString(plan.sessionID) || plan.peerName != "worker" ||
		!plan.context.groupsSpecified || strings.Join(plan.context.groups, ",") != "project,review" ||
		!plan.alwaysApprove || !plan.yoloSpecified {
		t.Fatalf("Claude peer plan = %+v", plan)
	}
	if _, err := parseClaudePeerArgs([]string{"--resume", "not-a-uuid"}); err == nil {
		t.Fatal("non-exact Claude resume target was accepted")
	}
	if _, err := parseClaudePeerArgs([]string{"--bare"}); err == nil || !strings.Contains(err.Error(), "use bare claude to opt out") {
		t.Fatalf("messageable Claude peer accepted --bare: %v", err)
	}
}

func TestPrepareClaudePeerProfileSeedsAccountAndOnboardingState(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	publicRoot := filepath.Join(home, ".claude")
	privateRoot := filepath.Join(root, "private")
	if err := os.MkdirAll(publicRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	public := map[string]any{
		"hasCompletedOnboarding": true,
		"lastOnboardingVersion":  "2.1.233",
		"theme":                  "dark",
		"installMethod":          "native",
		"oauthAccount":           map[string]any{"emailAddress": "private@example.invalid"},
		"projects":               map[string]any{"/secret": map[string]any{}},
	}
	body, _ := json.Marshal(public)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateRoot, ".claude.json"), []byte(`{"theme":"light"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudePeerProfile(privateRoot, claudeprofile.Source{
		ConfigRoot: publicRoot, StatePath: filepath.Join(home, ".claude.json"),
	}); err != nil {
		t.Fatal(err)
	}
	privateBody, err := os.ReadFile(filepath.Join(privateRoot, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var private map[string]any
	if err := json.Unmarshal(privateBody, &private); err != nil {
		t.Fatal(err)
	}
	completedOnboarding, completedOnboardingOK := private["hasCompletedOnboarding"].(bool)
	if !completedOnboardingOK || !completedOnboarding || private["lastOnboardingVersion"] != "2.1.233" ||
		private["theme"] != "light" || private["installMethod"] != "native" {
		t.Fatalf("private onboarding state = %#v", private)
	}
	account, _ := private["oauthAccount"].(map[string]any)
	if account["emailAddress"] != "private@example.invalid" {
		t.Fatalf("private profile account binding = %#v", private["oauthAccount"])
	}
	if _, copied := private["projects"]; copied {
		t.Fatal("private onboarding seed copied project metadata")
	}
}

func TestPrepareClaudePeerProfileRejectsNullPrivateOnboardingState(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	publicRoot := filepath.Join(home, ".claude")
	privateRoot := filepath.Join(root, "private")
	if err := os.MkdirAll(publicRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"hasCompletedOnboarding":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateRoot, ".claude.json"), []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudePeerProfile(privateRoot, claudeprofile.Source{
		ConfigRoot: publicRoot, StatePath: filepath.Join(home, ".claude.json"),
	}); err == nil {
		t.Fatal("null private Claude onboarding state was accepted")
	}
}

func TestClaudePeerEnvironmentUsesPrivateRegistryForNestedLanes(t *testing.T) {
	t.Setenv(agentRuntimeDirEnv, "/agent-runtime")
	environment := claudePeerEnvironment([]string{
		"CLAUDE_CONFIG_DIR=/public-native",
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR=/public-bridge",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/public-secure",
		"CLAUDE_CODE_SIMPLE=1",
		"CLAUDE_CODE_HARBOR_KITE=0",
	}, "/private", "/secure", "session-1")
	for _, expected := range []string{
		"CLAUDE_CONFIG_DIR=/private",
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR=/private",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/secure",
		"CLAUDE_CODE_HARBOR_KITE=1",
		"AGENT_SESSIONS_SESSION_ID=session-1",
		"AGENT_SESSIONS_PRODUCT=claude",
	} {
		if !slices.Contains(environment, expected) {
			t.Fatalf("Claude peer environment missing %q: %v", expected, environment)
		}
	}
	if slices.Contains(environment, "CLAUDE_CODE_SIMPLE=1") || slices.Contains(environment, "CLAUDE_CODE_HARBOR_KITE=0") {
		t.Fatalf("Claude peer environment retained inbox-suppressing values: %v", environment)
	}
}

func TestClaudePeerEnvironmentKeepsDefaultSecureStorageSentinel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	source, err := claudeprofile.CurrentSource()
	if err != nil {
		t.Fatal(err)
	}
	environment := claudePeerEnvironment([]string{
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/wrong",
	}, "/private", source.SecureConfig, "session-1")
	if !slices.Contains(environment, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") ||
		slices.Contains(environment, "CLAUDE_SECURESTORAGE_CONFIG_DIR=/wrong") {
		t.Fatalf("Claude peer secure-storage environment = %v", environment)
	}
}

func TestClaudePeerPrivateRegistryRegistersAndRestoresPreferences(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	runtimeDir := filepath.Join(root, "runtime")
	stateDir := filepath.Join(root, "agent-state")
	publicConfig := filepath.Join(root, "public-claude")
	if err := os.MkdirAll(publicConfig, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- federator.RunAgent(ctx, federator.AgentOptions{
			HostID: "host-test", HostName: "host-test", RuntimeDir: runtimeDir, StateDir: stateDir,
			ClaudeConfigDir: publicConfig, ScanInterval: 20 * time.Millisecond,
			Logger: log.New(io.Discard, "", 0),
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent: %v", err)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := federator.ReadAgentStatus(runtimeDir); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	helper := filepath.Join(root, "claude")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec \"" + testBinary + "\" -test.run=TestClaudePeerNativeHelper -- \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentRuntimeDirEnv, runtimeDir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state-home"))
	if err := os.MkdirAll(filepath.Join(root, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("CLAUDE_CONFIG_DIR", publicConfig)
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", helper)
	t.Setenv(claudePeerNativeHelperEnv, "1")
	plan, err := parseClaudePeerArgs([]string{"--group", "project", "--peer-name", "worker", "--dangerously-skip-permissions"})
	if err != nil {
		t.Fatal(err)
	}
	// Run the real public entry with the stable ID generated above so the test
	// can inspect the exact retained profile and durable catalog entry.
	if err := RunClaudePeer([]string{
		"--session-id", plan.sessionID, "--group", "project", "--peer-name", "worker", "--dangerously-skip-permissions",
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: plan.sessionID, Product: "claude",
	})
	if err != nil || !resolved.Preference.AlwaysApprove || !slices.Contains(resolved.EffectiveGroups, "project") {
		t.Fatalf("restored Claude preferences = %+v, %v", resolved, err)
	}
	privateRoot := claudePeerPrivateRoot("host-test", plan.sessionID)
	attachmentLock, err := acquireClaudePeerProfileLock(privateRoot)
	if err != nil {
		t.Fatal(err)
	}
	blockedErr := RunClaudePeer([]string{
		"--session-id", plan.sessionID, "--group", "replacement", "--no-yolo",
	})
	releaseClaudePeerProfileLock(attachmentLock)
	if blockedErr == nil {
		t.Fatal("concurrent Claude attachment unexpectedly launched")
	}
	unchanged, err := federator.ResolveSessionPreferences(runtimeDir, federator.ResolvePreferencesRequest{
		SessionID: plan.sessionID, Product: "claude",
	})
	if err != nil || !unchanged.Preference.AlwaysApprove || !slices.Contains(unchanged.EffectiveGroups, "project") ||
		slices.Contains(unchanged.EffectiveGroups, "replacement") {
		t.Fatalf("failed concurrent attachment mutated preferences: %+v, %v", unchanged, err)
	}
	entries, err := os.ReadDir(filepath.Join(privateRoot, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	serviceRows := 0
	serviceKeys := 0
	nativeRows := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".key") {
			body, readErr := os.ReadFile(filepath.Join(privateRoot, "sessions", entry.Name()))
			var key struct {
				PeerToken string `json:"peerToken"`
				ProcStart string `json:"procStart"`
			}
			if readErr != nil || json.Unmarshal(body, &key) != nil || len(key.PeerToken) != 32 || key.ProcStart == "" {
				t.Fatalf("invalid projected host-agent key %s: %s, %v", entry.Name(), body, readErr)
			}
			serviceKeys++
			continue
		}
		if filepath.Ext(entry.Name()) != ".json" {
			t.Fatalf("unexpected private Claude registry artifact %s", entry.Name())
		}
		body, _ := os.ReadFile(filepath.Join(privateRoot, "sessions", entry.Name()))
		var row map[string]any
		_ = json.Unmarshal(body, &row)
		if service, _ := row["agentService"].(bool); service {
			serviceRows++
		} else {
			nativeRows++
		}
	}
	if serviceRows != 1 || serviceKeys != 1 || nativeRows != 0 {
		t.Fatalf("private Claude registry has service=%d keys=%d native=%d; entries=%v", serviceRows, serviceKeys, nativeRows, entries)
	}
	status, err := federator.ReadAgentStatus(runtimeDir)
	if err != nil || status.LocalPeers != 0 {
		t.Fatalf("Claude peer was not unregistered: %+v, %v", status, err)
	}
}

func TestReadClaudeNativePeerRecordRequiresExactLiveIdentityAndSocket(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	start := federator.ProcessStart(pid)
	socket := filepath.Join(shortClaudePeerTestRoot(t), strconv.Itoa(pid)+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	row := claudeNativePeerRecord{
		PID: pid, SessionID: "session", ProcStart: start, Entrypoint: "cli", Kind: "interactive", MessagingSocketPath: socket,
	}
	body, _ := json.Marshal(row)
	path := filepath.Join(root, "sessions", strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readClaudeNativePeerRecord(root, pid, start); err != nil {
		t.Fatal(err)
	}
	row.ProcStart = "reused"
	body, _ = json.Marshal(row)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readClaudeNativePeerRecord(root, pid, start); err == nil {
		t.Fatal("stale native Claude row was accepted")
	}
}

func TestCleanupClaudePeerArtifactsRequiresPIDAbsence(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	socket := filepath.Join(shortClaudePeerTestRoot(t), strconv.Itoa(pid)+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	row := claudeNativePeerRecord{
		PID: pid, SessionID: "old-session", ProcStart: "old-start",
		MessagingSocketPath: socket, Entrypoint: "cli", Kind: "interactive",
	}
	body, _ := json.Marshal(row)
	record := filepath.Join(root, "sessions", strconv.Itoa(pid)+".json")
	if err := os.WriteFile(record, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupClaudePeerNativeArtifacts(root, row, row.ProcStart, row.SessionID); err == nil {
		t.Fatal("cleanup accepted a live or reused native PID")
	}
	if _, err := os.Lstat(record); err != nil {
		t.Fatalf("PID-reuse guard removed native record: %v", err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("PID-reuse guard removed native socket: %v", err)
	}
}

func TestClaudePeerProfileLockSerializesAttachments(t *testing.T) {
	root := filepath.Join(shortClaudePeerTestRoot(t), "config")
	first, err := acquireClaudePeerProfileLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireClaudePeerProfileLock(root); err == nil {
		releaseClaudePeerProfileLock(second)
		t.Fatal("second attachment acquired the same private profile")
	}
	releaseClaudePeerProfileLock(first)
	third, err := acquireClaudePeerProfileLock(root)
	if err != nil {
		t.Fatalf("released profile lock was not reusable: %v", err)
	}
	releaseClaudePeerProfileLock(third)
}

func TestPrepareClaudePeerAttachmentRejectsOrphanedLiveNativeRow(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	directory := filepath.Join(root, "sessions")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	row := claudeNativePeerRecord{
		PID: pid, SessionID: "stable-session", ProcStart: federator.ProcessStart(pid),
		MessagingSocketPath: filepath.Join(shortClaudePeerTestRoot(t), strconv.Itoa(pid)+".sock"),
	}
	body, _ := json.Marshal(row)
	if err := os.WriteFile(filepath.Join(directory, strconv.Itoa(pid)+".json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudePeerAttachment(root, row.SessionID); err == nil {
		t.Fatal("resume admitted beside an orphaned live native adapter")
	}
}

func TestCleanupClaudePeerArtifactsRemovesPreReadyUnknownModeRow(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	directory := filepath.Join(root, "sessions")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	const pid = 1_000_000_001
	socket := filepath.Join(shortClaudePeerTestRoot(t), strconv.Itoa(pid)+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	row := claudeNativePeerRecord{
		PID: pid, SessionID: "pre-ready", ProcStart: "old", PermissionMode: "future-mode",
		MessagingSocketPath: socket, Entrypoint: "cli", Kind: "interactive",
	}
	body, _ := json.Marshal(row)
	record := filepath.Join(directory, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(record, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupClaudePeerNativeArtifacts(root, claudeNativePeerRecord{PID: pid}, row.ProcStart, row.SessionID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{record, socket} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("pre-ready artifact survived cleanup: %s (%v)", path, err)
		}
	}
}

func TestCleanupClaudePeerArtifactsRemovesProvisionalRowWithoutSocket(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	directory := filepath.Join(root, "sessions")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	const pid = 1_000_000_002
	row := claudeNativePeerRecord{
		PID: pid, SessionID: "provisional", ProcStart: "old", Entrypoint: "cli", Kind: "interactive",
	}
	body, _ := json.Marshal(row)
	record := filepath.Join(directory, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(record, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupClaudePeerNativeArtifacts(root, claudeNativePeerRecord{PID: pid}, row.ProcStart, row.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(record); !os.IsNotExist(err) {
		t.Fatalf("provisional native row survived cleanup: %v", err)
	}
}

func TestCleanupClaudePeerArtifactsRemovesExactTokenSidecars(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	directory := filepath.Join(root, "sessions")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	const pid = 1_000_000_000
	socket := filepath.Join(shortClaudePeerTestRoot(t), strconv.Itoa(pid)+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	row := claudeNativePeerRecord{
		PID: pid, SessionID: "finished", ProcStart: "old", MessagingSocketPath: socket,
		Entrypoint: "cli", Kind: "interactive",
	}
	body, _ := json.Marshal(row)
	record := filepath.Join(directory, strconv.Itoa(pid)+".json")
	validKey := filepath.Join(directory, strconv.Itoa(pid)+"."+strings.Repeat("a", 64)+".key")
	invalidKey := filepath.Join(directory, strconv.Itoa(pid)+".not-a-token.key")
	for path, body := range map[string][]byte{record: body, validKey: []byte(`{"peerToken":"secret"}`), invalidKey: []byte("keep")} {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupClaudePeerNativeArtifacts(root, row, row.ProcStart, row.SessionID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{record, validKey, socket} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("owned artifact survived cleanup: %s (%v)", path, err)
		}
	}
	if _, err := os.Lstat(invalidKey); err != nil {
		t.Fatalf("unrecognized sidecar was removed: %v", err)
	}
}

func shortClaudePeerTestRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "cp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove short Claude peer test root: %v", err)
		}
	})
	return root
}
