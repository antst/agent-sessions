package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type bufferWriteCloser struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (*bufferWriteCloser) Close() error { return nil }

func (b *bufferWriteCloser) Write(body []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(body)
}

func (b *bufferWriteCloser) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *bufferWriteCloser) String() string { return string(b.Bytes()) }

func TestInspectClaudeAuthentication(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantLogged bool
		wantMethod string
		wantErr    string
	}{
		{
			name:       "authenticated subscription",
			body:       `printf '%s\n' 'update warning' >&2; printf '%s\n' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}'`,
			wantLogged: true,
			wantMethod: "claude.ai",
		},
		{
			name:       "documented logged out exit",
			body:       `printf '%s\n' '{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}'; exit 1`,
			wantMethod: "none",
		},
		{
			name:    "malformed status",
			body:    `printf '%s\n' 'not-json'`,
			wantErr: "decode Claude Code authentication status",
		},
		{
			name:    "unexpected command failure",
			body:    `printf '%s\n' '{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}'; exit 2`,
			wantErr: "authentication check for Claude Code failed",
		},
		{
			name:    "unsupported auth command diagnostic",
			body:    `printf '%s\n' 'unsupported-auth-status' >&2; exit 2`,
			wantErr: "unsupported-auth-status",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claudeBin := filepath.Join(t.TempDir(), "claude")
			script := "#!/bin/sh\n" +
				`[ "$*" = "auth status --json" ] || exit 97` + "\n" + test.body + "\n"
			if err := os.WriteFile(claudeBin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			status, err := inspectClaudeAuthentication(claudeBin)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("authentication error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || status.LoggedIn != test.wantLogged || status.AuthMethod != test.wantMethod {
				t.Fatalf("authentication status = %+v, %v", status, err)
			}
		})
	}
}

func TestClaudeLaneDoctorRequiresAuthentication(t *testing.T) {
	tests := []struct {
		name       string
		authStatus string
		authExit   int
		wantCode   int
		wantLogged bool
	}{
		{
			name:       "authenticated",
			authStatus: `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`,
			wantLogged: true,
		},
		{
			name:       "logged out",
			authStatus: `{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`,
			authExit:   1,
			wantCode:   1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
			claudeBin := filepath.Join(root, "claude")
			script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n  --version) printf '%%s\\n' '2.1.233' ;;\n  'auth status --json') printf '%%s\\n' '%s'; exit %d ;;\n  *) exit 97 ;;\nesac\n", test.authStatus, test.authExit)
			if err := os.WriteFile(claudeBin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			supervisorSocket := startAuthorizationControlServer(t, func(map[string]any) map[string]any {
				return map[string]any{}
			})
			t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
			t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
			t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)

			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stdout
			os.Stdout = write
			code, doctorErr := doctorClaudeLane()
			_ = write.Close()
			os.Stdout = original
			body, _ := io.ReadAll(read)
			_ = read.Close()
			if doctorErr != nil || code != test.wantCode {
				t.Fatalf("doctor = code %d err %v, body %s", code, doctorErr, body)
			}
			var report map[string]any
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatalf("decode doctor report: %v, body %s", err, body)
			}
			supervisorReachable, _ := report["supervisor_reachable"].(bool)
			if report["claude_logged_in"] != test.wantLogged || report["claude_version"] != "2.1.233" || !supervisorReachable {
				t.Fatalf("doctor report = %#v", report)
			}
			if test.wantLogged && report["claude_auth_error"] != nil {
				t.Fatalf("authenticated doctor reported auth error: %#v", report)
			}
			if !test.wantLogged && !strings.Contains(stringValue(report["claude_auth_error"]), "not authenticated") {
				t.Fatalf("logged-out doctor report = %#v", report)
			}
		})
	}
}

func TestClaudeLaneDoctorChecksSeededPrivateProfileAndDefaultCredentialNamespace(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{
  "hasCompletedOnboarding": true,
  "oauthAccount": {"accountUuid":"account-1","organizationUuid":"org-1"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeBin := filepath.Join(root, "claude")
	script := `#!/bin/sh
case "$*" in
  --version) printf '%s\n' '2.1.233' ;;
  'auth status --json')
    if test "${CLAUDE_SECURESTORAGE_CONFIG_DIR+x}:$CLAUDE_SECURESTORAGE_CONFIG_DIR" = 'x:' &&
       test "$CLAUDE_CONFIG_DIR" = "$CLAUDE_PEER_CLAUDE_CONFIG_DIR" &&
       test -z "${CLAUDECODE+x}${CLAUDE_CODE_SESSION_ID+x}${CLAUDE_PID+x}${CLAUDE_CODE_ENTRYPOINT+x}" &&
       grep -q '"oauthAccount"' "$CLAUDE_CONFIG_DIR/.claude.json"; then
      printf '%s\n' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}'
      exit 0
    fi
    printf '%s\n' '{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}'
    exit 1 ;;
  *) exit 97 ;;
esac
`
	if err := os.WriteFile(claudeBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	supervisorSocket := startAuthorizationControlServer(t, func(map[string]any) map[string]any {
		return map[string]any{}
	})
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-session")
	t.Setenv("CLAUDE_PID", "123")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "outer")
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "isolated-registry"))
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", supervisorSocket)

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	code, doctorErr := doctorClaudeLane()
	_ = write.Close()
	os.Stdout = original
	body, _ := io.ReadAll(read)
	_ = read.Close()
	if doctorErr != nil || code != 0 {
		t.Fatalf("private-profile doctor = code %d err %v, body %s", code, doctorErr, body)
	}
	var report map[string]any
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if !boolValue(report["claude_logged_in"]) || report["claude_auth_error"] != nil {
		t.Fatalf("private-profile doctor report = %#v", report)
	}
	paths := resolveNativePaths()
	matches, err := filepath.Glob(filepath.Join(profileDataRoot(paths), ".claude-doctor-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("doctor authentication profiles remain: %v, %v", matches, err)
	}
}

func TestParseClaudeLaneArgsLifecycleAndPolicy(t *testing.T) {
	options, err := parseClaudeLaneArgs([]string{
		"start", "--name", "reviewer", "--permission-mode", "dontAsk",
		"--max-budget-usd", "2.50", "--auto-archive-after", "5", "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.name != "reviewer" || options.permissionMode != "dontAsk" || options.maxBudgetUSD != "2.50" || options.autoArchiveDelay != 5*time.Second {
		t.Fatalf("unexpected options: %#v", options)
	}
	if _, err := parseClaudeLaneArgs([]string{"start", "--name", "bad", "--notify", "owner", "-"}); err == nil {
		t.Fatal("--notify without --persistent was accepted")
	}
	if resumed, err := parseClaudeLaneArgs([]string{"resume", "existing", "--notify", "owner", "-"}); err != nil || !resumed.notifyExplicit {
		t.Fatalf("persistent resume notification override was rejected before durable state resolution: %#v, %v", resumed, err)
	}
	if _, err := parseClaudeLaneArgs([]string{"wait", "lane", "--auto-archive-after", "5"}); err == nil {
		t.Fatal("wait accepted a lifecycle policy option")
	}
	if _, err := parseClaudeLaneArgs([]string{"start", "--name", "bad", "--permission-mode", "default", "-"}); err == nil {
		t.Fatal("unsupported Claude permission mode was accepted")
	}
	if _, err := parseClaudeLaneArgs([]string{"start", "--name", "bare", "--bare", "-"}); err == nil {
		t.Fatal("messageable Claude lane accepted --bare without a native peer socket")
	}
	listed, err := parseClaudeLaneArgs([]string{"list", "--mine", "--all"})
	if err != nil || !listed.mine || !listed.all {
		t.Fatalf("Claude list --mine options = %#v, %v", listed, err)
	}
	for _, args := range [][]string{
		{"status", "lane", "--timeout", "1"},
		{"wait", "lane", "--persistent"},
		{"archive", "lane", "--model", "claude"},
		{"list", "--no-auto-archive"},
		{"status", "lane", "--mine"},
	} {
		if _, err := parseClaudeLaneArgs(args); err == nil {
			t.Fatalf("ignored command option was accepted: %#v", args)
		}
	}
}

func TestClaudeLaneOwnerPermissionClassIsInheritedOnlyWhenImplicit(t *testing.T) {
	owner := laneOwner{PID: 123, ProcStart: "start", SessionID: "owner", PermissionMode: "bypassPermissions"}
	implicit := applyClaudeLaneOwnerContext(claudeLaneOptions{permissionMode: "dontAsk"}, owner)
	if implicit.permissionMode != "bypassPermissions" {
		t.Fatalf("implicit owner mode was not inherited: %#v", implicit)
	}
	explicit := applyClaudeLaneOwnerContext(claudeLaneOptions{
		permissionMode: "dontAsk", permissionModeSet: true,
	}, owner)
	if explicit.permissionMode != "dontAsk" {
		t.Fatalf("explicit lane mode was overwritten: %#v", explicit)
	}
}

func TestClaudeLaneOwnerPermissionClassRejectsUncorroboratedRegistryBypass(t *testing.T) {
	peer := peerSession{PID: os.Getpid(), ProcStart: readProcStart(os.Getpid()), PermissionMode: "bypassPermissions"}
	if got := corroboratedOwnerPermissionMode(peer, peer.PID); got != "default" {
		t.Fatalf("source-asserted bypass was trusted without argv corroboration: %q", got)
	}
}

func TestClaudeLaneCodexOwnerPeerRequiresMatchingPIDAndSession(t *testing.T) {
	peers := []peerSession{
		{PID: 101, ProcStart: "outer-start", SessionID: "outer", Entrypoint: "codex", MessagingSocketPath: "/tmp/outer.sock"},
		{PID: 202, ProcStart: "inner-start", SessionID: "inner", Entrypoint: "codex", MessagingSocketPath: "/tmp/inner.sock"},
	}
	peer, ok := matchingCodexOwnerPeer(peers, "inner")
	if !ok || peer.PID != 202 || peer.SessionID != "inner" {
		t.Fatalf("matching Codex owner peer = %#v, %v", peer, ok)
	}
	if codexBridgeStateMatchesPeer(map[string]any{
		"pid": 101, "procStart": "outer-start", "sessionId": "inner", "socketPath": "/tmp/inner.sock",
	}, peer) {
		t.Fatal("session ID was accepted without the matching bridge process identity")
	}
	if !codexBridgeStateMatchesPeer(map[string]any{
		"pid": 202, "procStart": "inner-start", "sessionId": "inner", "socketPath": "/tmp/inner.sock",
	}, peer) {
		t.Fatal("corroborated Codex bridge state was rejected")
	}
}

func TestClaudeLaneListMineFiltersByProcessIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	ownerPID, ownerProcStart := os.Getpid(), readProcStart(os.Getpid())
	states := []claudeLaneState{
		{Type: "claude-peer-lane", Name: "owned", ThreadID: "owned", SessionID: "owned", Status: "idle", OwnerPID: ownerPID, OwnerProcStart: ownerProcStart, CreatedAt: 1},
		{Type: "claude-peer-lane", Name: "owned-archived", ThreadID: "owned-archived", SessionID: "owned-archived", Status: "archived", OwnerPID: ownerPID, OwnerProcStart: ownerProcStart, CreatedAt: 1},
		{Type: "claude-peer-lane", Name: "foreign", ThreadID: "foreign", SessionID: "foreign", Status: "idle", OwnerPID: ownerPID, OwnerProcStart: "different-process-start", CreatedAt: 1},
		{Type: "claude-peer-lane", Name: "persistent", ThreadID: "persistent", SessionID: "persistent", Status: "idle", Persistent: true, OwnerPID: ownerPID, OwnerProcStart: ownerProcStart, CreatedAt: 1},
	}
	for _, state := range states {
		if err := writeClaudeLaneState(paths, state); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(all bool) string {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = write
		code, listErr := listClaudeLanes(claudeLaneOptions{
			mine: true, all: all, ownerPID: ownerPID, ownerProcStart: ownerProcStart,
		})
		_ = write.Close()
		os.Stdout = original
		body, _ := io.ReadAll(read)
		_ = read.Close()
		if code != 0 || listErr != nil {
			t.Fatalf("Claude list --mine all=%v: code=%d err=%v", all, code, listErr)
		}
		return string(body)
	}
	mine := capture(false)
	if !strings.Contains(mine, `"name":"owned"`) || strings.Contains(mine, "owned-archived") || strings.Contains(mine, `"name":"foreign"`) || strings.Contains(mine, `"name":"persistent"`) {
		t.Fatalf("Claude mine list = %s", mine)
	}
	mineAll := capture(true)
	if !strings.Contains(mineAll, `"name":"owned"`) || !strings.Contains(mineAll, "owned-archived") || strings.Contains(mineAll, `"name":"foreign"`) || strings.Contains(mineAll, `"name":"persistent"`) {
		t.Fatalf("Claude mine all list = %s", mineAll)
	}
}

func TestClaudeLaneWorkerEnvDropsInheritedIdentityAndEnablesMessaging(t *testing.T) {
	environment := claudeLaneWorkerEnv([]string{
		"PATH=/bin", "CLAUDE_PID=123", "CLAUDE_CODE_SESSION_ID=wrong",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=0", "CODEX_THREAD_ID=outer", "KEEP=yes",
		"CLAUDE_CODE_SIMPLE=1", "CLAUDE_CODE_HARBOR_KITE=0",
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR=/outer/config",
	}, "child-session", "/private/config", "/public/config")
	joined := strings.Join(environment, "\n")
	for _, expected := range []string{
		"PATH=/bin", "KEEP=yes", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
		"CLAUDE_CODE_HARBOR_KITE=1",
		"AGENT_SESSIONS_SESSION_ID=child-session", "AGENT_SESSIONS_PRODUCT=claude",
		"AGENT_SESSIONS_AGENT_RUNTIME_DIR=" + laneAgentRuntimeDir(),
		"CLAUDE_CONFIG_DIR=/private/config", "CLAUDE_PEER_CLAUDE_CONFIG_DIR=/private/config",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/public/config",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("worker environment = %#v; missing %s", environment, expected)
		}
	}
	if strings.Contains(joined, "CODEX_THREAD_ID=outer") || strings.Contains(joined, "CLAUDE_PEER_CLAUDE_CONFIG_DIR=/outer/config") ||
		strings.Contains(joined, "CLAUDE_CODE_SIMPLE=1") || strings.Contains(joined, "CLAUDE_CODE_HARBOR_KITE=0") {
		t.Fatalf("worker environment = %#v", environment)
	}
}

func TestClaudeLaneWorkerEnvKeepsDefaultSecureStorageSentinel(t *testing.T) {
	environment := claudeLaneWorkerEnv([]string{
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/wrong",
	}, "child-session", "/private/config", "")
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") ||
		strings.Contains(joined, "CLAUDE_SECURESTORAGE_CONFIG_DIR=/wrong") {
		t.Fatalf("worker secure-storage environment = %#v", environment)
	}
}

func TestPrepareClaudeLaneProfileSeedsAccountBinding(t *testing.T) {
	root := t.TempDir()
	publicRoot := filepath.Join(root, "public")
	privateRoot := filepath.Join(root, "private")
	statePath := filepath.Join(root, "source.json")
	if err := os.MkdirAll(publicRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{
  "hasCompletedOnboarding":true,
  "oauthAccount":{"accountUuid":"account-1"},
  "projects":{"secret":true}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentRuntimeDirEnvironment, filepath.Join(root, "no-agent"))
	if err := prepareClaudeLaneProfile(nativePaths{claudeRoot: publicRoot}, privateRoot, statePath); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(privateRoot, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]any
	if err := json.Unmarshal(body, &profile); err != nil {
		t.Fatal(err)
	}
	account, _ := profile["oauthAccount"].(map[string]any)
	if !boolValue(profile["hasCompletedOnboarding"]) || account["accountUuid"] != "account-1" {
		t.Fatalf("private Claude lane profile = %#v", profile)
	}
	if _, copied := profile["projects"]; copied {
		t.Fatalf("private Claude lane profile copied project state: %#v", profile)
	}
}

func TestClaudeLaneCapturesNativeWorkerWithoutRewritingPeer(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{
		claudeRoot: filepath.Join(root, "claude"), runtimeDir: filepath.Join(root, "runtime"),
	}
	if err := os.MkdirAll(filepath.Join(paths.claudeRoot, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.runtimeDir, "cc-socks"), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(paths.runtimeDir, "cc-socks", strconv.Itoa(os.Getpid())+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	procStart := readProcStart(os.Getpid())
	row := map[string]any{
		"pid": os.Getpid(), "procStart": procStart, "sessionId": "session-test",
		"entrypoint": "sdk-cli", "messagingSocketPath": socket,
	}
	registry := filepath.Join(paths.claudeRoot, "sessions", strconv.Itoa(os.Getpid())+".json")
	if err := writeJSONAtomic(registry, row); err != nil {
		t.Fatal(err)
	}
	manager := claudeLaneManager{paths: paths, state: claudeLaneState{
		SessionID: "session-test", WorkerPID: os.Getpid(), WorkerProcStart: procStart,
	}}
	if err := manager.captureWorkerPeer(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("native worker socket was rewritten: info=%v err=%v", info, err)
	}
	if manager.state.WorkerSocket != socket || readJSONMap(registry) == nil {
		t.Fatalf("native worker peer was not retained: %#v", manager.state)
	}
}

func TestClaudeLaneCaptureReportsWorkerExitDuringStartup(t *testing.T) {
	root := t.TempDir()
	manager := claudeLaneManager{
		paths: nativePaths{claudeRoot: filepath.Join(root, "claude"), runtimeDir: filepath.Join(root, "runtime")},
		state: claudeLaneState{
			SessionID: "session-test", WorkerPID: 4242, WorkerProcStart: "123",
		},
		workerDone: make(chan error, 1),
	}
	manager.workerDone <- errors.New("invalid model")
	started := time.Now()
	err := manager.captureWorkerPeer()
	if err == nil || !strings.Contains(err.Error(), "invalid model") {
		t.Fatalf("startup worker error was not preserved: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("dead worker was not detected promptly: %v", elapsed)
	}
}

func TestClaudeLanePeerCaptureDoesNotHoldManagerLockWhilePolling(t *testing.T) {
	root := t.TempDir()
	manager := &claudeLaneManager{
		paths:      nativePaths{claudeRoot: filepath.Join(root, "claude"), runtimeDir: filepath.Join(root, "runtime")},
		state:      claudeLaneState{SessionID: "session-test", WorkerPID: 4242, WorkerProcStart: "123"},
		workerDone: make(chan error, 1),
	}
	captureDone := make(chan error, 1)
	go func() { captureDone <- manager.captureWorkerPeer() }()
	time.Sleep(25 * time.Millisecond)
	lockAcquired := make(chan struct{})
	go func() {
		manager.mu.Lock()
		close(lockAcquired)
		manager.mu.Unlock()
	}()
	select {
	case <-lockAcquired:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Claude worker peer capture held the manager lock while polling")
	}
	manager.workerDone <- errors.New("stop polling")
	select {
	case err := <-captureDone:
		if err == nil || !strings.Contains(err.Error(), "stop polling") {
			t.Fatalf("capture completion = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Claude worker peer capture did not stop")
	}
}

func TestClaudeLaneReadinessBudgetCoversWorkerPeerPublication(t *testing.T) {
	if claudeLaneManagerReadyTimeout < 2*15*time.Second {
		t.Fatalf("Claude lane readiness budget %s does not cover worker publication", claudeLaneManagerReadyTimeout)
	}
}

func TestClaudeLaneMineDoesNotUseTransientParent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	t.Setenv("CLAUDE_PID", "")
	mine := withClaudeLaneLaunchContext(claudeLaneOptions{command: "list", mine: true})
	if mine.ownerPID != 0 || mine.ownerProcStart != "" {
		t.Fatalf("unresolved Claude --mine fell back to a transient parent: %+v", mine)
	}
	fallback := withClaudeLaneLaunchContext(claudeLaneOptions{command: "start"})
	if fallback.ownerPID != 0 || fallback.ownerProcStart != "" {
		t.Fatalf("ordinary shell unexpectedly became a Claude lifecycle owner: %+v", fallback)
	}
}

func TestNonpersistentLaneStartRequiresCapturedOwnerIdentity(t *testing.T) {
	if _, err := startLaneNative(laneOptions{command: "start", name: "missing-owner"}, false); err == nil || !strings.Contains(err.Error(), "cannot corroborate a stable lifecycle owner") {
		t.Fatalf("Codex lane owner capture error = %v", err)
	}
	if _, err := startClaudeLane(claudeLaneOptions{command: "start", name: "missing-owner"}, false); err == nil || !strings.Contains(err.Error(), "cannot corroborate a stable lifecycle owner") {
		t.Fatalf("Claude lane owner capture error = %v", err)
	}
}

func TestValidateLaneOwner(t *testing.T) {
	pid := os.Getpid()
	procStart := readProcStart(pid)
	if procStart == "" {
		t.Fatal("current process has no start token")
	}
	tests := []struct {
		name       string
		persistent bool
		pid        int
		procStart  string
		wantErr    bool
	}{
		{name: "nonpersistent exact match", pid: pid, procStart: procStart},
		{name: "nonpersistent empty token", pid: pid, wantErr: true},
		{name: "nonpersistent wrong token", pid: pid, procStart: procStart + "-wrong", wantErr: true},
		{name: "nonpersistent unknown equivalent", pid: 1 << 30, procStart: procStart, wantErr: true},
		{name: "persistent without owner", persistent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateLaneOwner(test.persistent, test.pid, test.procStart)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateLaneOwner(%v, %d, %q) error = %v, wantErr %v", test.persistent, test.pid, test.procStart, err, test.wantErr)
			}
		})
	}
}

func TestClaudeLaneResumePreservesPersistentLifecyclePolicyUnlessExplicitlyChanged(t *testing.T) {
	state := claudeLaneState{
		Persistent: true, OwnerPID: 91, OwnerProcStart: "old", OwnerSessionID: "old-session",
		NotifyTarget: "session:old-session",
	}
	applyClaudeLaneResumeOptions(&state, claudeLaneOptions{
		ownerPID: 92, ownerProcStart: "new", ownerSessionID: "",
	})
	if !state.Persistent || state.OwnerPID != 0 || state.OwnerProcStart != "" ||
		state.OwnerSessionID != "" || state.NotifyTarget != "session:old-session" {
		t.Fatalf("implicit resume changed persistent lifecycle = %+v", state)
	}
	state = claudeLaneState{
		OwnerPID: 91, OwnerProcStart: "old", OwnerSessionID: "old-session",
		NotifyTarget: "session:old-session",
	}
	applyClaudeLaneResumeOptions(&state, claudeLaneOptions{
		ownerPID: 92, ownerProcStart: "new", ownerSessionID: "new-session",
	})
	if state.Persistent || state.OwnerPID != 92 || state.OwnerProcStart != "new" ||
		state.OwnerSessionID != "new-session" || state.NotifyTarget != "session:new-session" {
		t.Fatalf("implicit parent-owned resume lifecycle = %+v", state)
	}
	applyClaudeLaneResumeOptions(&state, claudeLaneOptions{persistent: true, persistentSet: true})
	if !state.Persistent || state.OwnerPID != 0 || state.OwnerProcStart != "" || state.OwnerSessionID != "" {
		t.Fatalf("persistent resume lifecycle = %+v", state)
	}
}

func TestClaudeLaneFailedRestartRestoresNonArchivedState(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), profileRoot: filepath.Join(root, "profile")}
	original := claudeLaneState{
		Type: "claude-peer-lane", Name: "recoverable", ThreadID: "session-recoverable", SessionID: "session-recoverable",
		Status: "idle", Persistent: true, Turns: []claudeLaneTurn{{ID: "turn-old", Status: "completed", Collected: true}},
		TurnID: "turn-old", LatestTurnID: "turn-old",
	}
	starting := original
	starting.Status, starting.StartupID = "starting", "startup-failed"
	starting.Turns = append(starting.Turns, claudeLaneTurn{ID: "turn-rejected", Status: "queued"})
	starting.TurnID, starting.LatestTurnID = "turn-rejected", "turn-rejected"
	if err := writeClaudeLaneState(paths, starting); err != nil {
		t.Fatal(err)
	}
	rollbackClaudeLaneResume(paths, original, starting.StartupID, 0)
	restored, err := readClaudeLaneState(paths, original.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != original.Status || restored.StartupID != "" || restored.TurnID != original.TurnID ||
		restored.LatestTurnID != original.LatestTurnID || len(restored.Turns) != 1 {
		t.Fatalf("failed non-archived restart was not recoverable: %+v", restored)
	}
}

func TestClaudeLaneMaintenanceRetriesFailedArchiveReservation(t *testing.T) {
	root := t.TempDir()
	blockedRoot := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedRoot, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: nativePaths{profileRoot: blockedRoot},
		state: claudeLaneState{
			SessionID: "session-retry-archive", Status: "idle", OwnerPID: 1 << 30,
			OwnerProcStart: "stale-owner", Persistent: false,
		},
		done: make(chan struct{}),
	}
	if manager.maintain() {
		t.Fatal("failed archive reservation incorrectly stopped maintenance")
	}
	if manager.closing {
		t.Fatal("failed archive reservation left manager closing")
	}
	select {
	case <-manager.done:
		t.Fatal("failed archive reservation closed manager completion")
	default:
	}
}

func TestClaudeLaneLegacyManagerArgsRequireExactRoleAndSession(t *testing.T) {
	for _, runtimeName := range []string{"agent-session-runtime", "codex-messaging"} {
		valid := []string{"/opt/" + runtimeName, "claude-lane-manager", "--session-id", "session-old"}
		if !processArgsIdentifyClaudeLaneManager(valid, "session-old") {
			t.Fatalf("exact %s manager arguments were rejected", runtimeName)
		}
	}
	for _, args := range [][]string{
		{"/opt/agent-session-runtime", "shim", "--session-id", "session-old"},
		{"/opt/agent-session-runtime", "claude-lane-manager", "--session-id", "session-other"},
		{"/opt/not-agent-session-runtime", "claude-lane-manager", "--session-id", "session-old"},
	} {
		if processArgsIdentifyClaudeLaneManager(args, "session-old") {
			t.Fatalf("unrelated process arguments were accepted: %q", args)
		}
	}
}

func TestLiveClaudeLaneResumeAdoptsValidatedPlainParent(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	ownerPID := os.Getpid()
	ownerProcStart := readProcStart(ownerPID)
	if ownerProcStart == "" {
		t.Fatal("current process has no start token")
	}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "live-resume", ThreadID: "live-resume", SessionID: "live-resume",
		Status: "idle", Persistent: true, OwnerPID: 91, OwnerProcStart: "old", OwnerSessionID: "old-session",
		NotifyTarget: "session:old-session",
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1), done: make(chan struct{}),
	}
	_, err := manager.handleControl(map[string]any{
		"action": "resume", "sessionId": state.SessionID,
		"persistent": false, "ownerPid": ownerPID, "ownerProcStart": ownerProcStart, "ownerSessionId": "",
		"notifySet": true, "notifyTarget": "",
		"turn": claudeLaneTurn{ID: "turn-new", Prompt: "work", Status: "queued", CreatedAt: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manager.state.Persistent || manager.state.OwnerPID != ownerPID || manager.state.OwnerProcStart != ownerProcStart ||
		manager.state.OwnerSessionID != "" || manager.state.NotifyTarget != "" {
		t.Fatalf("live parent-owned resume lifecycle = %+v", manager.state)
	}
}

func TestLiveClaudeLaneResumeRejectsUncorroboratedOwnerBeforeMutation(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "live-resume", ThreadID: "live-resume", SessionID: "live-resume",
		Status: "idle", Persistent: true, NotifyTarget: "existing",
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1), done: make(chan struct{}),
	}
	_, err := manager.handleControl(map[string]any{
		"action": "resume", "sessionId": state.SessionID,
		"persistent": false, "ownerPid": os.Getpid(), "ownerProcStart": "wrong",
		"notifySet": true, "notifyTarget": "changed",
		"turn": claudeLaneTurn{ID: "turn-new", Prompt: "work", Status: "queued", CreatedAt: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot corroborate a stable lifecycle owner") {
		t.Fatalf("uncorroborated live resume error = %v", err)
	}
	if !manager.state.Persistent || manager.state.NotifyTarget != "existing" || len(manager.state.Turns) != 0 {
		t.Fatalf("uncorroborated live resume mutated state: %+v", manager.state)
	}
}

func TestNonpersistentLaneResumeRequiresCapturedOwnerBeforeDurableMutation(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "state")
	t.Setenv("CLAUDE_PEER_DATA_DIR", dataRoot)
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	if err := recordLaneState(paths, laneState{
		Type: "codex-peer-lane", Name: "nonpersistent-codex", ThreadID: "nonpersistent-codex",
		SessionID: "nonpersistent-codex", Status: "completed", Persistent: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeLaneState(paths, claudeLaneState{
		Type: "claude-peer-lane", Name: "nonpersistent-claude", ThreadID: "nonpersistent-claude",
		SessionID: "nonpersistent-claude", Status: "completed", Persistent: false,
	}); err != nil {
		t.Fatal(err)
	}
	statePaths := []string{
		laneStatePath(paths, "nonpersistent-codex"),
		claudeLaneStatePath(paths, "nonpersistent-claude"),
	}
	before := map[string][]byte{}
	for _, path := range statePaths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = body
	}
	for name, resume := range map[string]func() (int, error){
		"codex": func() (int, error) {
			return resumeLaneNative(laneOptions{command: "resume", target: "nonpersistent-codex", promptFile: filepath.Join(root, "missing-prompt")})
		},
		"claude": func() (int, error) {
			return resumeClaudeLane(claudeLaneOptions{command: "resume", target: "nonpersistent-claude", promptFile: filepath.Join(root, "missing-prompt")})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resume(); err == nil || !strings.Contains(err.Error(), "cannot corroborate a stable lifecycle owner") {
				t.Fatalf("resume owner error = %v", err)
			}
			for _, path := range statePaths {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, before[path]) {
					t.Fatalf("resume mutated lifecycle state before owner validation: %s, err=%v", path, err)
				}
			}
		})
	}
}

func TestProcessArgsLookLikeCodexHostExcludesBridgeLaunchers(t *testing.T) {
	for _, args := range [][]string{
		{"/tmp/agent-session-runtime", "claude-lane", "run"},
		{"/bin/bash", "-c", "codex-peer-lane start --name child"},
		{"/home/user/.local/bin/codex-peer", "--version"},
	} {
		if processArgsLookLikeCodexHost(args) {
			t.Fatalf("bridge launcher classified as Codex host: %#v", args)
		}
	}
	for _, args := range [][]string{{"/opt/codex", "app-server"}, {"node", "/opt/openai/codex.js"}} {
		if !processArgsLookLikeCodexHost(args) {
			t.Fatalf("Codex host not recognized: %#v", args)
		}
	}
}

func TestClaudeLaneManagerAdoptsNativePeerTurn(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile"), claudeRoot: filepath.Join(root, "claude")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Cwd: root, Status: "idle", AutoArchive: true, AutoArchiveDelayMS: 60_000,
		PermissionMode: "dontAsk", CreatedAt: time.Now().UnixMilli(),
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, done: make(chan struct{}), workerDone: make(chan error, 1),
		writeQueue: make(chan claudeLaneWrite, 1),
	}
	manager.handleWorkerFrame(map[string]any{
		"type": "command_lifecycle", "state": "started", "command_uuid": "peer-turn-1",
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "peer-turn-1",
		"message": map[string]any{"role": "user", "content": "peer prompt"},
		"origin":  map[string]any{"kind": "peer", "msg_id": "message-1"},
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "first answer",
	}, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"first answer"}`))
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Turns) != 1 || latest.Turns[0].ID != "peer-turn-1" || latest.Turns[0].MessageID != "message-1" ||
		latest.Turns[0].Prompt != "peer prompt" || latest.Turns[0].Outcome != "completed" || latest.AutoArchiveAt == 0 {
		t.Fatalf("latest state = %#v", latest)
	}
}

func TestClaudeLanePeerMessageSteersActiveTurnWithoutChangingItsIdentity(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	local := claudeLaneTurn{ID: "local-turn", Prompt: "local prompt", Status: "active", StartedAt: 1}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", Turns: []claudeLaneTurn{local}, TurnID: local.ID, CreatedAt: 1,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1)}
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "peer-steer",
		"message": map[string]any{"role": "user", "content": "steer prompt"},
		"origin":  map[string]any{"kind": "peer", "msg_id": "steer-message"},
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "command_lifecycle", "state": "started", "command_uuid": "peer-steer",
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "combined answer",
	}, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"combined answer"}`))
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Turns) != 1 || latest.Turns[0].ID != local.ID || latest.Turns[0].Prompt != "local prompt" ||
		latest.Turns[0].MessageID != "" || latest.Turns[0].Result != "combined answer" {
		t.Fatalf("peer steer changed active turn identity: %#v", latest.Turns)
	}
}

func TestClaudeLanePeerTurnWinsRaceBeforeSubmittedLocalTurnStarts(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	local := claudeLaneTurn{ID: "local-turn", Prompt: "local prompt", Status: "queued", CreatedAt: 1}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "idle", PermissionMode: "dontAsk", Turns: []claudeLaneTurn{local}, TurnID: local.ID,
		AutoArchive: true, AutoArchiveDelayMS: 60_000, CreatedAt: 1,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1)}
	manager.mu.Lock()
	if err := manager.startNextTurnLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	if manager.state.Turns[0].Status != "submitted" {
		t.Fatalf("local turn was marked active before its stream replay: %#v", manager.state.Turns[0])
	}
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "peer-turn",
		"message": map[string]any{"role": "user", "content": "peer prompt"},
		"origin":  map[string]any{"kind": "peer", "msg_id": "peer-message"},
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "command_lifecycle", "state": "started", "command_uuid": "peer-turn",
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "peer answer",
	}, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"peer answer"}`))
	if deadline := manager.state.Turns[0].DeadlineAt; deadline <= time.Now().UnixMilli() {
		t.Fatalf("peer completion did not refresh the submitted-turn watchdog: %#v", manager.state.Turns[0])
	}
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "sdk-local-replay",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "local prompt"}}},
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "local answer",
	}, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"local answer"}`))
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Turns) != 2 || latest.Turns[0].ID != local.ID || latest.Turns[0].Result != "local answer" ||
		latest.Turns[1].ID != "peer-turn" || latest.Turns[1].Result != "peer answer" || latest.AutoArchiveAt == 0 {
		t.Fatalf("race results were attributed to the wrong turns: %#v", latest)
	}
}

func TestClaudeLaneLocalReplayCollapsesProvisionalPeerSteer(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", TurnID: "local-turn", CreatedAt: 1,
		Turns: []claudeLaneTurn{
			{ID: "local-turn", Prompt: "local prompt", Status: "submitted", DeadlineAt: time.Now().Add(time.Second).UnixMilli()},
			{ID: "peer-turn", Prompt: "peer steer", MessageID: "peer-message", Status: "active", StartedAt: 1},
		},
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1), interruptRequested: "peer-turn",
	}
	// Claude emits command_lifecycle started even when a later launcher replay
	// steers this peer command into the launcher turn. The lifecycle frame must
	// not make the provisional peer record non-collapsible.
	manager.handleWorkerFrame(map[string]any{
		"type": "command_lifecycle", "state": "started", "command_uuid": "peer-turn",
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "sdk-local-replay",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "local prompt"}}},
	}, nil)
	if len(manager.state.Turns) != 1 || manager.state.Turns[0].ID != "local-turn" || manager.state.Turns[0].Status != "active" ||
		manager.interruptRequested != "local-turn" {
		t.Fatalf("local replay did not collapse the provisional peer steer: %#v", manager.state.Turns)
	}
	manager.interruptRequested = ""
	manager.handleWorkerFrame(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "combined answer",
	}, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"combined answer"}`))
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Turns) != 1 || latest.Turns[0].ID != "local-turn" || latest.Turns[0].Result != "combined answer" {
		t.Fatalf("combined result was not attributed to the launcher turn: %#v", latest.Turns)
	}
}

func TestClaudeLaneToolResultUserFrameDoesNotActivateSubmittedTurn(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", TurnID: "local-turn", CreatedAt: 1,
		Turns: []claudeLaneTurn{
			{ID: "local-turn", Prompt: "local prompt", Status: "submitted", DeadlineAt: time.Now().Add(time.Second).UnixMilli()},
			{ID: "peer-turn", Prompt: "peer steer", MessageID: "peer-message", Status: "active", StartedAt: 1},
		},
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1)}
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "tool-result-frame",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "tool-1", "content": "local prompt",
		}}},
	}, nil)
	if len(manager.state.Turns) != 2 || manager.state.Turns[0].Status != "submitted" || manager.state.Turns[1].Status != "active" {
		t.Fatalf("tool_result user frame altered turn arbitration: %#v", manager.state.Turns)
	}
}

func TestClaudeLaneLocalReplayUsesFIFOInsteadOfByteEquality(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", TurnID: "local-turn", CreatedAt: 1,
		Turns: []claudeLaneTurn{{
			ID: "local-turn", Prompt: "local prompt\n", Status: "submitted", DeadlineAt: time.Now().Add(time.Second).UnixMilli(),
		}},
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1)}
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "sdk-local-replay",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "local prompt"},
			map[string]any{"type": "text", "text": "<system-reminder>normalized replay</system-reminder>"},
			map[string]any{"type": "tool_result", "tool_use_id": "irrelevant", "content": "ignored"},
		}},
	}, nil)
	if manager.state.Turns[0].Status != "active" || manager.state.Turns[0].Prompt != "local prompt\n" {
		t.Fatalf("normalized replay did not activate the sole submitted turn: %#v", manager.state.Turns[0])
	}
}

func TestClaudeLaneResultCompletesTheSoleSubmittedTurn(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", TurnID: "local-turn", CreatedAt: 1,
		Turns: []claudeLaneTurn{{
			ID: "local-turn", Prompt: "local prompt", Status: "submitted", DeadlineAt: time.Now().Add(time.Second).UnixMilli(),
		}},
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1)}
	manager.handleWorkerFrame(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "launcher answer",
	}, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"launcher answer"}`))
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Turns[0].Status != "completed" || latest.Turns[0].Result != "launcher answer" || latest.Turns[0].Outcome != "completed" {
		t.Fatalf("sole submitted launcher turn did not collect its result: %#v", latest.Turns[0])
	}
}

func TestClaudeLaneSubmittedTurnWatchdogFailsClosed(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", TurnID: "local-turn", CreatedAt: 1,
		Turns: []claudeLaneTurn{{
			ID: "local-turn", Prompt: "local prompt", Status: "submitted", DeadlineAt: time.Now().Add(-time.Second).UnixMilli(),
		}},
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, done: make(chan struct{}), writeQueue: make(chan claudeLaneWrite, 1)}
	if !manager.maintain() {
		t.Fatal("expired submitted turn did not stop the manager")
	}
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "archived" || latest.Turns[0].Status != "failed" || latest.Turns[0].Outcome != "failed" ||
		!strings.Contains(latest.Turns[0].Error, "did not acknowledge") {
		t.Fatalf("submitted-turn watchdog did not fail closed: %#v", latest)
	}
}

func TestClaudeLaneCollectorAcknowledgementSurvivesManagerPersist(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	turn := claudeLaneTurn{ID: "turn-1", Prompt: "x", Status: "completed", Outcome: "completed", CompletedAt: 1}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Cwd: root, Status: "idle", Turns: []claudeLaneTurn{turn}, TurnID: turn.ID, CreatedAt: 1,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := acknowledgeClaudeLaneTurn(paths, state.SessionID, turn.ID); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, workerInput: &bufferWriteCloser{}}
	if err := manager.persistLocked(); err != nil {
		t.Fatal(err)
	}
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !latest.Turns[0].Collected || latest.CollectedTurnID != turn.ID {
		t.Fatalf("collector acknowledgement was reverted: %#v", latest)
	}
}

func TestClaudeLaneAutoArchiveRechecksNewTurnReservation(t *testing.T) {
	root := t.TempDir()
	state := claudeLaneState{
		Type: "claude-peer-lane", SessionID: "session-test", Status: "idle",
		AutoArchive: true, AutoArchiveAt: time.Now().Add(-time.Second).UnixMilli(),
	}
	manager := &claudeLaneManager{
		paths: nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")},
		state: state, done: make(chan struct{}),
	}
	manager.mu.Lock()
	manager.state.AutoArchiveAt = 0
	manager.state.Status = "active"
	manager.mu.Unlock()
	manager.shutdown("auto-archive delay elapsed", false)
	if manager.closing || manager.state.Status == "archived" {
		t.Fatalf("stale auto-archive claim retired a new turn: %#v", manager.state)
	}
}

func TestClaudeLaneArchivedManagerRejectsWake(t *testing.T) {
	root := t.TempDir()
	manager := &claudeLaneManager{
		paths: nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")},
		state: claudeLaneState{Type: "claude-peer-lane", SessionID: "session-test", Status: "archived"},
	}
	manager.handleWorkerFrame(map[string]any{
		"type": "command_lifecycle", "state": "started", "command_uuid": "late-turn",
	}, nil)
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "late-turn",
		"message": map[string]any{"role": "user", "content": "late prompt"},
		"origin":  map[string]any{"kind": "peer", "msg_id": "late-message"},
	}, nil)
	if len(manager.state.Turns) != 0 {
		t.Fatal("archived manager adopted a native peer turn")
	}
}

func TestClaudeLaneEmptyToolsFlagIsPreservedInWorkerArgv(t *testing.T) {
	options, err := parseClaudeLaneArgs([]string{"start", "--name", "tool-free", "--tools", "", "-"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.toolsSet || options.tools != "" {
		t.Fatalf("empty --tools lost explicitness: %#v", options)
	}
	args := claudeLaneWorkerArgs(claudeLaneState{
		SessionID: "session-test", Name: "tool-free", PermissionMode: "dontAsk", ToolsSet: true,
	})
	found := false
	for index := range args {
		if args[index] == "--tools" && index+1 < len(args) && args[index+1] == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("worker argv omitted explicit empty tools: %#v", args)
	}
	if !containsArgValue(args, "--allowedTools", "SendMessage,ListAgents") {
		t.Fatalf("worker argv did not allow lane messaging tools: %#v", args)
	}
	if !containsArgValue(args, "--settings", `{"crossSessionInbound":"accept"}`) {
		t.Fatalf("worker argv did not accept native inbound lane messages: %#v", args)
	}
	if !containsArgValue(args, "--name", "tool-free") {
		t.Fatalf("worker argv did not preserve the lane's outbound sender name: %#v", args)
	}
	resumeArgs := claudeLaneWorkerArgs(claudeLaneState{
		SessionID: "session-test", Name: "tool-free", PermissionMode: "dontAsk", WorkerSessionStarted: true,
	})
	if !containsString(resumeArgs, "--resume") || containsString(resumeArgs, "--session-id") {
		t.Fatalf("durably started worker did not resume its transcript: %#v", resumeArgs)
	}
}

func TestClaudeLaneWorkerMergesMessagingIntoExplicitAllowedTools(t *testing.T) {
	args := claudeLaneWorkerArgs(claudeLaneState{
		SessionID: "session-test", Name: "reviewer", PermissionMode: "dontAsk",
		AllowedTools: "Read,Bash(git *)", AllowedToolsSet: true,
	})
	if !containsArgValue(args, "--allowedTools", "Read,Bash(git *),SendMessage,ListAgents") {
		t.Fatalf("worker argv replaced caller allowed tools: %#v", args)
	}
}

func TestClaudeLaneWorkerNormalizesEmptyExplicitAllowedTools(t *testing.T) {
	args := claudeLaneWorkerArgs(claudeLaneState{
		SessionID: "claude-session-test", Name: "lane", PermissionMode: "dontAsk",
		AllowedToolsSet: true,
	})
	if !containsArgValue(args, "--allowedTools", "SendMessage,ListAgents") {
		t.Fatalf("expected native messaging tools without an empty leading entry, got %q", args)
	}
}

func containsArgValue(args []string, flag, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag && args[index+1] == value {
			return true
		}
	}
	return false
}

func TestClaudeLaneArchivedResumePolicyUpdatesOnlyExplicitFields(t *testing.T) {
	state := claudeLaneState{
		PermissionMode: "bypassPermissions", Model: "old", Tools: "Bash", ToolsSet: true,
		AllowedTools: "Read", AllowedToolsSet: true, Bare: true,
	}
	applyClaudeLaneWorkerOptions(&state, claudeLaneOptions{model: "new", modelSet: true})
	if state.Model != "new" || state.PermissionMode != "bypassPermissions" || state.Tools != "Bash" || !state.ToolsSet || state.AllowedTools != "Read" || !state.Bare {
		t.Fatalf("partial policy update reset unrelated fields: %#v", state)
	}
}

func TestClaudeLaneWorkerNeverRestartsLegacyBareMode(t *testing.T) {
	args := claudeLaneWorkerArgs(claudeLaneState{
		SessionID: "session-test", Name: "legacy-bare", PermissionMode: "dontAsk", Bare: true,
	})
	if containsString(args, "--bare") {
		t.Fatalf("current worker argv restarted a non-messageable legacy bare lane: %#v", args)
	}
	if err := validateClaudeLaneResumeState(claudeLaneState{Bare: true}); err == nil || !strings.Contains(err.Error(), "legacy lane used --bare") {
		t.Fatalf("legacy bare resume did not fail with an actionable error: %v", err)
	}
}

func TestClaudeLaneCompactsOnlyCollectedTurnHistory(t *testing.T) {
	state := claudeLaneState{}
	for index := 0; index < claudeLaneRetainedCollectedTurns+6; index++ {
		state.Turns = append(state.Turns, claudeLaneTurn{
			ID: fmt.Sprintf("collected-%02d", index), Collected: true,
			ResultFrame: json.RawMessage(`{"large":"payload"}`),
		})
	}
	state.Turns = append(state.Turns,
		claudeLaneTurn{ID: "pending-a", ResultFrame: json.RawMessage(`{"keep":true}`)},
		claudeLaneTurn{ID: "pending-b", ResultFrame: json.RawMessage(`{"keep":true}`)},
	)
	compactClaudeLaneTurns(&state)
	if len(state.Turns) != claudeLaneRetainedCollectedTurns+2 || state.Turns[0].ID != "collected-06" {
		t.Fatalf("compacted turn history = %#v", state.Turns)
	}
	for _, turn := range state.Turns[:claudeLaneRetainedCollectedTurns] {
		if !turn.Collected || len(turn.ResultFrame) != 0 {
			t.Fatalf("collected turn retained raw result: %#v", turn)
		}
	}
	for _, turn := range state.Turns[claudeLaneRetainedCollectedTurns:] {
		if turn.Collected || len(turn.ResultFrame) == 0 {
			t.Fatalf("uncollected turn was compacted: %#v", turn)
		}
	}
}

func TestClaudeLaneManagerMergesAcknowledgementsBeforeCompaction(t *testing.T) {
	manager := &claudeLaneManager{}
	latest := claudeLaneState{}
	for index := 0; index < claudeLaneRetainedCollectedTurns+6; index++ {
		turn := claudeLaneTurn{
			ID: fmt.Sprintf("collected-%02d", index), ResultFrame: json.RawMessage(`{"large":"payload"}`),
		}
		manager.state.Turns = append(manager.state.Turns, turn)
		turn.Collected = true
		latest.Turns = append(latest.Turns, turn)
	}
	latest.CollectedTurnID = "collected-69"
	manager.mergeCollectionAcknowledgementsLocked(latest)
	if len(manager.state.Turns) != claudeLaneRetainedCollectedTurns || manager.state.Turns[0].ID != "collected-06" {
		t.Fatalf("merged turn history = %#v", manager.state.Turns)
	}
	for _, turn := range manager.state.Turns {
		if !turn.Collected || len(turn.ResultFrame) != 0 {
			t.Fatalf("acknowledged turn was not compacted: %#v", turn)
		}
	}
}

func TestClaudeLaneCleanupRemovesOnlyItsDeadNativeWorkerPeer(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(map[string]any)
		wantRemove bool
	}{
		{name: "matching", mutate: func(map[string]any) {}, wantRemove: true},
		{name: "foreign-session", mutate: func(row map[string]any) { row["sessionId"] = "other-session" }},
		{name: "foreign-entrypoint", mutate: func(row map[string]any) { row["entrypoint"] = "interactive" }},
		{name: "foreign-process-start", mutate: func(row map[string]any) { row["procStart"] = "other-start" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runtimeRoot, err := os.MkdirTemp("", "cr-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(runtimeRoot); err != nil {
					t.Errorf("remove compact runtime root: %v", err)
				}
			})
			paths := nativePaths{claudeRoot: filepath.Join(root, "claude"), runtimeDir: runtimeRoot}
			if err := os.MkdirAll(filepath.Join(paths.claudeRoot, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(paths.runtimeDir, "cc-socks"), 0700); err != nil {
				t.Fatal(err)
			}
			const deadPID = 1_000_000_000
			socket := filepath.Join(paths.runtimeDir, "cc-socks", strconv.Itoa(deadPID)+".sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			registry := filepath.Join(paths.claudeRoot, "sessions", strconv.Itoa(deadPID)+".json")
			row := map[string]any{
				"pid": deadPID, "procStart": "old-start", "sessionId": "lane-session",
				"entrypoint": "sdk-cli", "messagingSocketPath": socket,
			}
			test.mutate(row)
			if err := writeJSONAtomic(registry, row); err != nil {
				t.Fatal(err)
			}
			cleanupClaudeNativeWorkerPeer(paths, claudeLaneState{
				SessionID: "lane-session", WorkerPID: deadPID, WorkerProcStart: "old-start", WorkerSocket: socket,
			})
			_, registryErr := os.Stat(registry)
			_, socketErr := os.Lstat(socket)
			if test.wantRemove {
				if !os.IsNotExist(registryErr) || !os.IsNotExist(socketErr) {
					t.Fatalf("matching worker residue survived: registry=%v socket=%v", registryErr, socketErr)
				}
			} else if registryErr != nil || socketErr != nil {
				t.Fatalf("foreign worker residue was removed: registry=%v socket=%v", registryErr, socketErr)
			}
		})
	}
}

func TestClaudeLaneManagerCaptureFailureDoesNotPersistOwner(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{profileRoot: filepath.Join(root, "profile"), runtimeDir: filepath.Join(root, "runtime")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "capture-failure", ThreadID: "capture-manager",
		SessionID: "capture-manager", Cwd: root, Status: "starting",
		ControlSocket: filepath.Join(root, "manager.sock"), PermissionMode: "dontAsk",
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state,
		captureProcStart: func(int) (string, error) { return "", errors.New("probe unavailable") },
	}
	if err := manager.start(); err == nil || !strings.Contains(err.Error(), "capture Claude lane manager identity") {
		t.Fatalf("manager capture failure = %v", err)
	}
	if _, err := os.Lstat(state.ControlSocket); !os.IsNotExist(err) {
		t.Fatalf("manager socket survived failed identity capture: %v", err)
	}
	persisted, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil || persisted.ManagerPID != 0 || persisted.ManagerProcStart != "" {
		t.Fatalf("manager identity persisted after failed capture: %+v, %v", persisted, err)
	}
}

func TestClaudeLaneWorkerCaptureFailureReapsChildWithoutPersistence(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	claudeBin := filepath.Join(root, "fake-claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PEER_CLAUDE_BIN", claudeBin)
	paths := nativePaths{profileRoot: filepath.Join(root, "profile")}
	workerPID := 0
	workerProcStart := ""
	t.Cleanup(func() {
		if workerPID > 1 && workerProcStart != "" && exactProcessIdentityMatch(workerPID, workerProcStart) {
			_ = syscall.Kill(workerPID, syscall.SIGKILL)
		}
	})
	manager := &claudeLaneManager{
		paths: paths,
		state: claudeLaneState{
			Type: "claude-peer-lane", Name: "worker-capture-failure", ThreadID: "capture-worker",
			SessionID: "capture-worker", Cwd: root, Status: "starting", PermissionMode: "dontAsk",
		},
		captureProcStart: func(pid int) (string, error) {
			workerPID = pid
			workerProcStart = readProcStart(pid)
			return "", errors.New("probe unavailable")
		},
	}
	if err := manager.startWorker(); err == nil || !strings.Contains(err.Error(), "capture Claude lane worker identity") {
		t.Fatalf("worker capture failure = %v", err)
	}
	if workerPID <= 1 || manager.worker != nil || manager.workerInput != nil ||
		manager.state.WorkerPID != 0 || manager.state.WorkerProcStart != "" {
		t.Fatalf("worker ownership survived failed capture: pid=%d manager=%+v", workerPID, manager.state)
	}
	if observation := observeProcessIdentity(workerPID, ""); observation.Status != processIdentityStale {
		t.Fatalf("worker was not reaped after failed identity capture: %+v", observation)
	}
	if _, err := os.Stat(claudeLaneStatePath(paths, manager.state.SessionID)); !os.IsNotExist(err) {
		t.Fatalf("worker state persisted before identity capture: %v", err)
	}
}

func TestClaudeLaneResumeRejectsOutstandingCollectionDebt(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "idle", Persistent: true, NotifyTarget: "existing", AutoArchive: true, AutoArchiveDelayMS: 60_000,
		Turns: []claudeLaneTurn{{ID: "turn-owed", Status: "completed", Outcome: "completed"}}, TurnID: "turn-owed",
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1), done: make(chan struct{}),
	}
	beforeManager, _ := json.Marshal(manager.state)
	beforeState, readErr := readClaudeLaneState(paths, state.SessionID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	beforeDurable, _ := json.Marshal(beforeState)
	_, err := manager.handleControl(map[string]any{
		"action": "resume", "sessionId": "session-test",
		"persistent": true, "autoArchive": false, "notifySet": true, "notifyTarget": "changed",
		"turn": claudeLaneTurn{ID: "turn-new", Prompt: "new", Status: "queued"},
	})
	if err == nil || !strings.Contains(err.Error(), "collect outstanding Claude lane turn turn-owed") {
		t.Fatalf("resume with genuine collection debt error = %v", err)
	}
	afterManager, _ := json.Marshal(manager.state)
	if !bytes.Equal(afterManager, beforeManager) {
		t.Fatalf("resume with genuine debt mutated manager state:\nbefore %s\nafter  %s", beforeManager, afterManager)
	}
	latest, readErr := readClaudeLaneState(paths, state.SessionID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	afterDurable, _ := json.Marshal(latest)
	if !bytes.Equal(afterDurable, beforeDurable) {
		t.Fatalf("resume with genuine debt mutated durable state:\nbefore %s\nafter  %s", beforeDurable, afterDurable)
	}
}

func TestLiveClaudeLaneResumeRefreshesExternalCollectionAcknowledgement(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	turn := claudeLaneTurn{ID: "turn-collected", Prompt: "peer work", Status: "completed", Outcome: "completed"}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "idle", Persistent: true, Turns: []claudeLaneTurn{turn}, TurnID: turn.ID,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, writeQueue: make(chan claudeLaneWrite, 1), done: make(chan struct{}),
	}
	if err := acknowledgeClaudeLaneTurn(paths, state.SessionID, turn.ID); err != nil {
		t.Fatal(err)
	}
	response, err := manager.handleControl(map[string]any{
		"action": "resume", "sessionId": state.SessionID,
		"turn": claudeLaneTurn{ID: "turn-new", Prompt: "new work", Status: "queued", CreatedAt: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response["turnId"] != "turn-new" || response["delivery"] != "started" {
		t.Fatalf("live resume response = %#v", response)
	}
	if len(manager.state.Turns) != 2 || !manager.state.Turns[0].Collected ||
		manager.state.CollectedTurnID != turn.ID || manager.state.Turns[1].ID != "turn-new" {
		t.Fatalf("manager did not merge external collection acknowledgement: %#v", manager.state)
	}
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Turns) != 2 || !latest.Turns[0].Collected || latest.CollectedTurnID != turn.ID ||
		latest.Turns[1].ID != "turn-new" || latest.Turns[1].Status != "submitted" {
		t.Fatalf("live resume durable state = %#v", latest)
	}
}

func TestClaudeLaneArchiveMarksClosedBeforeAcknowledgement(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "idle", PermissionMode: "dontAsk", CreatedAt: time.Now().UnixMilli(),
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: state, done: make(chan struct{})}
	if _, err := manager.handleControl(map[string]any{"action": "archive", "sessionId": state.SessionID}); err != nil {
		t.Fatal(err)
	}
	if !manager.closing || manager.state.Status != "archived" {
		t.Fatalf("archive returned before reserving shutdown: %#v", manager.state)
	}
	manager.handleWorkerFrame(map[string]any{
		"type": "command_lifecycle", "state": "started", "command_uuid": "late-turn",
	}, nil)
	if len(manager.state.Turns) != 0 {
		t.Fatal("native peer turn was accepted after archive acknowledgement")
	}
	manager.finishShutdown(false)
}

func TestClaudeLaneWorkerExitPreservesTimeoutOutcome(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	turn := claudeLaneTurn{ID: "turn-timeout", Prompt: "slow", Status: "active", StartedAt: 1}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", Turns: []claudeLaneTurn{turn}, TurnID: turn.ID, CreatedAt: 1,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, done: make(chan struct{}), timeoutRequested: turn.ID,
	}
	manager.handleWorkerExit(errors.New("signal: interrupt"))
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Turns[0].Outcome != "timed_out" || latest.Turns[0].Exit != 124 {
		t.Fatalf("timeout was degraded on worker exit: %#v", latest.Turns[0])
	}
}

func TestClaudeLaneWorkerExitRetriesFailedArchivePersistence(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	turn := claudeLaneTurn{ID: "turn-exit", Prompt: "work", Status: "active", StartedAt: 1}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "active", PermissionMode: "dontAsk", Turns: []claudeLaneTurn{turn}, TurnID: turn.ID, CreatedAt: 1,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	statePath := claudeLaneStatePath(paths, state.SessionID)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, done: make(chan struct{}), workerDone: make(chan error, 1),
	}
	if manager.handleWorkerExit(errors.New("worker failed")) {
		t.Fatal("worker exit reported cleanup complete after archive persistence failed")
	}
	select {
	case <-manager.done:
		t.Fatal("manager stopped after archive persistence failed")
	default:
	}
	if manager.state.Status != "archived" {
		t.Fatalf("in-memory archive was not retained for retry: %#v", manager.state)
	}
	if err := os.RemoveAll(statePath); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	if !manager.maintain() {
		t.Fatal("maintenance did not finish the retried durable archive")
	}
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "archived" || latest.Turns[0].Outcome != "failed" {
		t.Fatalf("retried archive lost worker-exit state: %#v", latest)
	}
	select {
	case <-manager.done:
	default:
		t.Fatal("manager remained live after the archive retry completed")
	}
}

func TestClaudeLaneManagerAdoptsPeerUserBeforeLifecycle(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile"), claudeRoot: filepath.Join(root, "claude")}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "idle", PermissionMode: "dontAsk", CreatedAt: time.Now().UnixMilli(),
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{
		paths: paths, state: state, done: make(chan struct{}),
		writeQueue: make(chan claudeLaneWrite, 1),
	}
	manager.handleWorkerFrame(map[string]any{
		"type": "user", "uuid": "peer-turn-fallback",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "recover me"},
		}},
		"origin": map[string]any{"kind": "peer", "msg_id": "message-fallback"},
	}, nil)
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Turns) != 1 || latest.Turns[0].ID != "peer-turn-fallback" ||
		latest.Turns[0].MessageID != "message-fallback" || latest.Turns[0].Prompt != "recover me" || latest.Turns[0].Status != "active" {
		t.Fatalf("peer user frame was not adopted exactly once: %#v", latest.Turns)
	}
}

func TestClaudeLaneManagerCannotOverwriteArchivedState(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	archived := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "archived", PermissionMode: "dontAsk", CreatedAt: 1,
	}
	if err := writeClaudeLaneState(paths, archived); err != nil {
		t.Fatal(err)
	}
	manager := &claudeLaneManager{paths: paths, state: archived}
	manager.state.Status = "active"
	if err := manager.persistLocked(); err == nil {
		t.Fatal("stale manager resurrected an archived lane")
	}
}

func TestClaudeLaneUnknownCleanupPreservesStateAndTransport(t *testing.T) {
	useBridgeTestAgent(t)
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	process := exec.Command("/bin/sleep", "60")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	sessionID := "00000000-0000-0000-0000-000000000c01"
	controlSocket := filepath.Join(root, "manager-control.sock")
	shimSocket := filepath.Join(root, "worker-shim.sock")
	workerAlias := filepath.Join(root, "worker-alias.sock")
	for _, path := range []string{controlSocket, shimSocket} {
		if err := os.WriteFile(path, []byte("owned"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(shimSocket, workerAlias); err != nil {
		t.Fatal(err)
	}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "unknown-cleanup", ThreadID: sessionID, SessionID: sessionID,
		Status: "archived", Persistent: true, ManagerPID: process.Process.Pid,
		// An empty start token is deliberately Unknown, never proof of death.
		ControlSocket: controlSocket, ShimSocket: shimSocket, WorkerSocketAlias: workerAlias,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	statePath := claudeLaneStatePath(paths, sessionID)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	assertPreserved := func(operation string) {
		t.Helper()
		after, readErr := os.ReadFile(statePath)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("%s changed durable state under unknown identity: %v", operation, readErr)
		}
		for _, path := range []string{controlSocket, shimSocket, workerAlias} {
			if _, statErr := os.Lstat(path); statErr != nil {
				t.Fatalf("%s removed transport %s under unknown identity: %v", operation, path, statErr)
			}
		}
	}
	if err := forceArchiveClaudeLane(paths, sessionID, "test unknown identity"); err == nil ||
		!strings.Contains(err.Error(), "cannot currently corroborate") {
		t.Fatalf("force archive unknown identity error = %v", err)
	}
	assertPreserved("force archive")
	prompt := filepath.Join(root, "prompt.txt")
	if err := os.WriteFile(prompt, []byte("resume"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resumeClaudeLane(claudeLaneOptions{
		command: "resume", target: sessionID, promptFile: prompt, persistent: true,
	}); err == nil || !strings.Contains(err.Error(), "cannot currently corroborate") {
		t.Fatalf("resume unknown identity error = %v", err)
	}
	assertPreserved("resume")
}

func TestClaudeLaneStaleCleanupRemovesOwnedTransport(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), claudeRoot: filepath.Join(root, "claude")}
	controlSocket := filepath.Join(root, "manager-control.sock")
	shimSocket := filepath.Join(root, "worker-shim.sock")
	workerAlias := filepath.Join(root, "worker-alias.sock")
	for _, path := range []string{controlSocket, shimSocket} {
		if err := os.WriteFile(path, []byte("owned"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(shimSocket, workerAlias); err != nil {
		t.Fatal(err)
	}
	state := claudeLaneState{
		ManagerPID: 1 << 30, ManagerProcStart: "definitely-stale",
		ControlSocket: controlSocket, ShimSocket: shimSocket, WorkerSocketAlias: workerAlias,
	}
	if err := cleanupClaudeLaneResidue(paths, state, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{controlSocket, workerAlias} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("stale cleanup retained %s: %v", path, err)
		}
	}
}

func TestClaudeLaneDeadLegacyIdentityCleanupRemovesOwnedTransport(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state"), claudeRoot: filepath.Join(root, "claude")}
	if err := os.MkdirAll(filepath.Join(paths.claudeRoot, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	controlSocket := filepath.Join(root, "legacy-manager-control.sock")
	workerAlias := filepath.Join(root, "legacy-worker-alias.sock")
	shimSocket := filepath.Join(root, "legacy-shim.sock")
	if err := os.WriteFile(controlSocket, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shimSocket, workerAlias); err != nil {
		t.Fatal(err)
	}
	workerPID := 1<<30 - 1
	state := claudeLaneState{
		SessionID:  "legacy-session",
		ManagerPID: 1 << 30, ManagerProcStart: "",
		WorkerPID: workerPID, WorkerProcStart: "",
		ControlSocket: controlSocket, ShimSocket: shimSocket, WorkerSocketAlias: workerAlias,
	}
	registry := filepath.Join(paths.claudeRoot, "sessions", strconv.Itoa(workerPID)+".json")
	if err := writeJSONAtomic(registry, map[string]any{
		"pid": workerPID, "procStart": "legacy-second-token", "sessionId": state.SessionID, "entrypoint": "sdk-cli",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cleanupClaudeLaneResidue(paths, state, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{controlSocket, workerAlias, registry} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("dead legacy cleanup retained %s: %v", path, err)
		}
	}
}

func TestClaudeLaneCleanupEscalatesManagerBlockedOnLifecycleLock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	paths := resolveNativePaths()
	sessionID := "00000000-0000-0000-0000-000000000c02"
	ready := filepath.Join(root, "manager-ready")
	command := exec.Command(os.Args[0], "-test.run=^TestClaudeLaneCleanupLockHelper$")
	command.Env = append(os.Environ(),
		"CLAUDE_CLEANUP_LOCK_HELPER=1",
		"CLAUDE_CLEANUP_LOCK_SESSION="+sessionID,
		"CLAUDE_CLEANUP_LOCK_READY="+ready,
		"CLAUDE_PEER_DATA_DIR="+paths.dataRoot,
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR="+paths.claudeRoot,
		"CODEX_HOME="+paths.codexHome,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blocking manager helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started, err := captureProcessStart(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	controlSocket := filepath.Join(root, "manager-control.sock")
	shimSocket := filepath.Join(root, "worker-shim.sock")
	workerAlias := filepath.Join(root, "worker-alias.sock")
	for _, path := range []string{controlSocket, shimSocket} {
		if err := os.WriteFile(path, []byte("owned"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(shimSocket, workerAlias); err != nil {
		t.Fatal(err)
	}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "blocked-manager", ThreadID: sessionID, SessionID: sessionID,
		Status: "active", ManagerPID: command.Process.Pid, ManagerProcStart: started,
		ControlSocket: controlSocket, ShimSocket: shimSocket, WorkerSocketAlias: workerAlias,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	if err := forceArchiveClaudeLane(paths, sessionID, "test blocked manager"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed < 1900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("blocked manager escalation took %s", elapsed)
	}
	latest, err := readClaudeLaneState(paths, sessionID)
	if err != nil || latest.Status != "archived" || latest.ManagerPID != 0 {
		t.Fatalf("blocked manager cleanup state = %#v, %v", latest, err)
	}
	for _, path := range []string{controlSocket, workerAlias} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("blocked manager cleanup retained %s: %v", path, err)
		}
	}
}

func TestClaudeLaneCleanupLockHelper(t *testing.T) {
	if os.Getenv("CLAUDE_CLEANUP_LOCK_HELPER") != "1" {
		return
	}
	paths := resolveNativePaths()
	sessionID := os.Getenv("CLAUDE_CLEANUP_LOCK_SESSION")
	ready := os.Getenv("CLAUDE_CLEANUP_LOCK_READY")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	<-signals
	lock, err := lockLaneLifecycle(paths, "claude-"+sessionID)
	if err == nil {
		unlockLaneLifecycle(lock)
	}
}

func TestClaudeLaneReconcileUsesFreshStartingTimestamp(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: root, profileRoot: filepath.Join(root, "profile"),
		runtimeDir: filepath.Join(root, "runtime"), claudeRoot: filepath.Join(root, "claude"),
	}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "starting", PermissionMode: "dontAsk", CreatedAt: time.Now().Add(-time.Hour).UnixMilli(),
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	reconcileClaudeLaneManagers(paths)
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "starting" {
		t.Fatalf("fresh resume reservation was treated as an orphan: %#v", latest)
	}
	latest.UpdatedAt = time.Now().Add(-time.Minute).UnixMilli()
	if err := writeJSONAtomic(claudeLaneStatePath(paths, latest.SessionID), latest); err != nil {
		t.Fatal(err)
	}
	reconcileClaudeLaneManagers(paths)
	latest, err = readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "archived" {
		t.Fatalf("stale starting reservation was not reclaimed: %#v", latest)
	}
}

func TestClaudeLaneFullWriteQueueTerminalizesTurn(t *testing.T) {
	root := t.TempDir()
	paths := nativePaths{dataRoot: root, profileRoot: filepath.Join(root, "profile")}
	turn := claudeLaneTurn{ID: "turn-queued", Prompt: "work", Status: "queued", CreatedAt: 1}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "idle", PermissionMode: "dontAsk", Turns: []claudeLaneTurn{turn}, TurnID: turn.ID, CreatedAt: 1,
	}
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	queue := make(chan claudeLaneWrite, 1)
	queue <- claudeLaneWrite{turnID: "occupied"}
	manager := &claudeLaneManager{paths: paths, state: state, writeQueue: queue, done: make(chan struct{})}
	manager.mu.Lock()
	err := manager.startNextTurnLocked()
	manager.mu.Unlock()
	if err == nil {
		t.Fatal("full worker queue did not surface an error")
	}
	latest, readErr := readClaudeLaneState(paths, state.SessionID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if latest.Turns[0].Status != "failed" || latest.Turns[0].Outcome != "failed" {
		t.Fatalf("full worker queue wedged an active turn: %#v", latest.Turns[0])
	}
}

func TestClaudeLaneStatusBindsOutcomeToReportedTurn(t *testing.T) {
	state := claudeLaneState{
		SessionID: "session-test", ThreadID: "session-test", TurnID: "turn-old", TerminalOutcome: "completed",
		Turns: []claudeLaneTurn{
			{ID: "turn-old", Status: "failed", Outcome: "failed", Exit: 1},
			{ID: "turn-new", Status: "completed", Outcome: "completed", Exit: 0},
		},
	}
	event := claudeLaneStatusEvent(state)
	if stringValue(event["outcome"]) != "failed" || intValue(event["exit"]) != 1 || stringValue(event["turn_status"]) != "failed" {
		t.Fatalf("status mixed two turns: %#v", event)
	}
}

func TestClaudeLaneArchivedNoticeRetriesAcrossReconcileTicks(t *testing.T) {
	runtimeDir := useBridgeTestAgent(t)
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "data"), profileRoot: filepath.Join(root, "profile"),
		claudeRoot: filepath.Join(root, "claude"), runtimeDir: filepath.Join(root, "runtime"),
	}
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "archived", PermissionMode: "dontAsk", WorkerSocket: filepath.Join(root, "dead-source.sock"), CreatedAt: 1,
		Notices: []claudeLaneNotice{{ID: "notice-retry", TurnID: "turn-1", Target: "session:target-session", Message: "terminal", CreatedAt: 1}},
	}
	_, stopParent := registerBridgeTestParent(t, runtimeDir)
	prepareBridgeTestLaneParent(t, runtimeDir, state.SessionID, "target-session")
	stopParent()
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	reconcileClaudeLaneManagers(paths)
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Notices[0].SentAt != 0 {
		t.Fatal("unreachable orphan notice was falsely acknowledged")
	}
	received, _ := registerBridgeTestParent(t, runtimeDir)
	reconcileClaudeLaneManagers(paths)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("orphan notice was not retried on a later reconciliation")
	}
	latest, err = readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Notices[0].SentAt == 0 {
		t.Fatal("retried orphan notice was not acknowledged")
	}
}

func TestClaudeLaneOrphanNoticeUsesVirtualSender(t *testing.T) {
	runtimeDir := useBridgeTestAgent(t)
	root := t.TempDir()
	paths := nativePaths{
		dataRoot: filepath.Join(root, "data"), profileRoot: filepath.Join(root, "profile"),
		claudeRoot: filepath.Join(root, "claude"), runtimeDir: filepath.Join(root, "runtime"),
	}
	targetSession := "target-session"
	state := claudeLaneState{
		Type: "claude-peer-lane", Name: "worker", ThreadID: "session-test", SessionID: "session-test",
		Status: "idle", PermissionMode: "dontAsk", WorkerSocket: filepath.Join(root, "dead-source.sock"), CreatedAt: 1,
		Notices: []claudeLaneNotice{{ID: "notice-1", TurnID: "turn-1", Target: "session:" + targetSession, Message: "terminal", CreatedAt: 1}},
	}
	received, _ := registerBridgeTestParent(t, runtimeDir)
	prepareBridgeTestLaneParent(t, runtimeDir, state.SessionID, targetSession)
	if err := writeClaudeLaneState(paths, state); err != nil {
		t.Fatal(err)
	}
	flushOrphanClaudeLaneNotices(paths, state.SessionID)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("orphan notice was not delivered")
	}
	latest, err := readClaudeLaneState(paths, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Notices[0].SentAt == 0 {
		t.Fatal("orphan notice delivery was not acknowledged durably")
	}
}

func TestClaudeLaneNoticeCarriesStableID(t *testing.T) {
	manager := &claudeLaneManager{state: claudeLaneState{
		Name: "worker", SessionID: "session-test", NotifyTarget: "session:owner",
	}}
	turn := claudeLaneTurn{ID: "turn-test", Status: "completed", Outcome: "completed"}
	manager.queueTerminalNoticeLocked(turn)
	if len(manager.state.Notices) != 1 {
		t.Fatalf("notice count = %d", len(manager.state.Notices))
	}
	notice := manager.state.Notices[0]
	if !strings.Contains(notice.Message, "notice="+notice.ID+" ") {
		t.Fatalf("terminal pointer omits stable notice id: %q", notice.Message)
	}
}

func TestClaudeLaneNoticeLockDoesNotBlockReconciliation(t *testing.T) {
	paths := nativePaths{profileRoot: t.TempDir()}
	first, err := lockClaudeLaneNotices(paths, "session-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockLaneStateFile(first)
	started := time.Now()
	second, err := lockClaudeLaneNotices(paths, "session-test")
	if second != nil {
		unlockLaneStateFile(second)
	}
	if err == nil {
		t.Fatal("contended notice lock unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("contended notice lock blocked for %s", elapsed)
	}
}
