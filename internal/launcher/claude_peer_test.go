package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const claudePeerNativeHelperEnv = "AGENT_SESSIONS_TEST_CLAUDE_PEER_NATIVE"
const claudePeerNativeFailBeforeRowEnv = "AGENT_SESSIONS_TEST_CLAUDE_PEER_FAIL_BEFORE_ROW"
const claudePeerNativeFailCleanupEnv = "AGENT_SESSIONS_TEST_CLAUDE_PEER_FAIL_CLEANUP"
const claudePeerNativeFailSettingsCleanupEnv = "AGENT_SESSIONS_TEST_CLAUDE_PEER_FAIL_SETTINGS_CLEANUP"

func TestClaudePeerNativeHelper(_ *testing.T) {
	if os.Getenv(claudePeerNativeHelperEnv) != "1" {
		return
	}
	if os.Getenv(claudePeerNativeFailBeforeRowEnv) == "1" {
		os.Exit(74)
	}
	sessionID := ""
	permissionMode := "default"
	nativeArgs := false
	for index, argument := range os.Args {
		if argument == "--" {
			if !nativeArgs {
				nativeArgs = true
				continue
			}
			break
		}
		if !nativeArgs {
			continue
		}
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
	if os.Getenv(claudePeerNativeFailCleanupEnv) == "1" {
		_ = os.Remove(socket)
		_ = os.WriteFile(socket, []byte("changed type"), 0o600)
	}
	if settingsPath := os.Getenv(claudePeerNativeFailSettingsCleanupEnv); settingsPath != "" {
		_ = os.Remove(settingsPath)
		_ = os.Mkdir(settingsPath, 0o700)
	}
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

func TestClaudePeerManagedOptionsStayBeforePromptDelimiter(t *testing.T) {
	plan, err := parseClaudePeerArgs([]string{"--peer-name", "worker", "--", "prompt", "--session-id", "not-an-option"})
	if err != nil {
		t.Fatal(err)
	}
	delimiter := slices.Index(plan.args, "--")
	if delimiter < 5 || plan.args[delimiter+1] != "prompt" ||
		!containsClaudeArgValue(plan.args[:delimiter], "--session-id", plan.sessionID) ||
		!containsClaudeArgValue(plan.args[:delimiter], "--name", "worker") ||
		!slices.Contains(plan.args[:delimiter], "--no-chrome") {
		t.Fatalf("managed Claude args crossed prompt delimiter: %v", plan.args)
	}
	withPermission := insertClaudeManagedArgs(plan.args, "--permission-mode", "default")
	permissionIndex := slices.Index(withPermission, "--permission-mode")
	if permissionIndex < 0 || permissionIndex >= slices.Index(withPermission, "--") {
		t.Fatalf("managed Claude permission crossed prompt delimiter: %v", withPermission)
	}
	for _, explicit := range []string{"--chrome", "--no-chrome"} {
		explicitPlan, parseErr := parseClaudePeerArgs([]string{explicit})
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if countString(explicitPlan.args, explicit) != 1 ||
			(explicit == "--chrome" && slices.Contains(explicitPlan.args, "--no-chrome")) {
			t.Fatalf("explicit Chrome mode was not preserved: %v", explicitPlan.args)
		}
	}
	promptPlan, err := parseClaudePeerArgs([]string{"--", "prompt", "--chrome"})
	if err != nil || !slices.Contains(beforeDoubleDash(promptPlan.args), "--no-chrome") {
		t.Fatalf("prompt text incorrectly opted into Chrome: %v, %v", promptPlan.args, err)
	}
}

func countString(values []string, wanted string) int {
	count := 0
	for _, value := range values {
		if value == wanted {
			count++
		}
	}
	return count
}

func containsClaudeArgValue(args []string, flag, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag && args[index+1] == value {
			return true
		}
	}
	return false
}

func TestPrepareClaudePeerLaunchSettingsMergesWithoutChangingSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "operator-settings.json")
	if err := os.WriteFile(source, []byte(`{"theme":"dark","crossSessionInbound":"reject"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(source)
	args, err := prepareClaudePeerLaunchSettings([]string{"--settings", source, "--model", "sonnet"}, filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := ""
	for index, argument := range args {
		if argument == "--settings" && index+1 < len(args) {
			settingsPath = args[index+1]
		}
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if json.Unmarshal(body, &settings) != nil || settings["theme"] != "dark" || settings["crossSessionInbound"] != "accept" {
		t.Fatalf("managed launch settings = %s", body)
	}
	info, _ := os.Stat(settingsPath)
	after, _ := os.ReadFile(source)
	if info == nil || info.Mode().Perm() != 0o600 || string(after) != string(before) {
		t.Fatalf("settings overlay mode/source changed: %v, %q", info, after)
	}
}

func TestPlanClaudePeerLaunchSettingsDoesNotMaterializeBeforePreparation(t *testing.T) {
	root := t.TempDir()
	args, body, err := planClaudePeerLaunchSettings([]string{"--settings", `{"theme":"dark"}`}, root)
	if err != nil || len(body) == 0 || !slices.Contains(args, filepath.Join(root, "launch-settings.json")) {
		t.Fatalf("planned settings = args=%v body=%s err=%v", args, body, err)
	}
	if _, err := os.Stat(filepath.Join(root, "launch-settings.json")); !os.IsNotExist(err) {
		t.Fatalf("settings were materialized before durable preparation: %v", err)
	}
}

func TestClaudePeerEnvironmentUsesSharedRegistryForNestedLanes(t *testing.T) {
	t.Setenv(agentRuntimeDirEnv, "/agent-runtime")
	environment := claudePeerEnvironment([]string{
		"CLAUDE_CONFIG_DIR=/shared",
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR=/wrong",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/secure",
		"CLAUDE_CODE_SIMPLE=1",
		"CLAUDE_CODE_HARBOR_KITE=0",
	}, "/shared", claudeprofile.Source{
		ConfigRoot: "/shared", ConfigEnvSet: true, ConfigEnvValue: "/shared",
		SecureConfig: "/secure", SecureEnvSet: true,
	}, "session-1")
	for _, expected := range []string{
		"CLAUDE_CONFIG_DIR=/shared",
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR=/shared",
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

func TestClaudePeerEnvironmentPreservesUnsetDefaultProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	_ = os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	source, err := claudeprofile.CurrentSource()
	if err != nil {
		t.Fatal(err)
	}
	environment := claudePeerEnvironment([]string{"PATH=/bin"}, source.ConfigRoot, source, "session-1")
	if slices.Contains(environment, "CLAUDE_CONFIG_DIR=") || slices.Contains(environment, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") {
		t.Fatalf("Claude peer secure-storage environment = %v", environment)
	}
}

func TestClaudePeerRejectsRegistryOrCredentialNamespaceMismatch(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	status := federator.AgentStatus{
		RegistryDir:        filepath.Join(shared, "sessions"),
		ClaudeConfigEnvSet: true, ClaudeConfigEnvValue: shared,
		ClaudeSecureEnvSet: true, ClaudeSecureConfig: "secure-a",
	}
	matching := claudeprofile.Source{
		ConfigRoot: shared, ConfigEnvSet: true, ConfigEnvValue: shared,
		SecureEnvSet: true, SecureConfig: "secure-a",
	}
	if err := requireClaudePeerProfileMatch(matching, status); err != nil {
		t.Fatalf("matching profile rejected: %v", err)
	}
	for name, source := range map[string]claudeprofile.Source{
		"registry": {
			ConfigRoot: filepath.Join(root, "other"), ConfigEnvSet: true,
			ConfigEnvValue: filepath.Join(root, "other"), SecureEnvSet: true, SecureConfig: "secure-a",
		},
		"config spelling": {
			ConfigRoot: shared, ConfigEnvSet: true, ConfigEnvValue: shared + "/../shared",
			SecureEnvSet: true, SecureConfig: "secure-a",
		},
		"secure namespace": {
			ConfigRoot: shared, ConfigEnvSet: true, ConfigEnvValue: shared,
			SecureEnvSet: true, SecureConfig: "secure-b",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := requireClaudePeerProfileMatch(source, status); err == nil {
				t.Fatal("mismatched native profile was accepted")
			}
		})
	}
}

func TestClaudePeerLaunchSettingsPreserveLargeJSONNumbers(t *testing.T) {
	root := t.TempDir()
	args, err := prepareClaudePeerLaunchSettings([]string{
		"--settings", `{"large":900719925474099312345,"nested":{"n":18446744073709551615}}`,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := ""
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--settings" {
			settingsPath = args[index+1]
			break
		}
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("900719925474099312345")) ||
		!bytes.Contains(body, []byte("18446744073709551615")) {
		t.Fatalf("settings numbers changed during merge: %s", body)
	}
}

func TestClaudePeerSharedRegistryRegistersAndRestoresPreferences(t *testing.T) {
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
	assertCatalogAbsent := func(sessionID string) {
		t.Helper()
		if preference, lookupErr := federator.LookupSessionPreferences(runtimeDir, sessionID); lookupErr == nil {
			t.Fatalf("rejected Claude peer mutated catalog: %+v", preference)
		}
	}
	const mismatchID = "00000000-0000-4000-8000-000000000201"
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "wrong-profile"))
	if err := RunClaudePeer([]string{"--session-id", mismatchID, "--group", "must-not-persist"}); err == nil {
		t.Fatal("mismatched Claude profile unexpectedly launched")
	}
	assertCatalogAbsent(mismatchID)
	t.Setenv("CLAUDE_CONFIG_DIR", publicConfig)

	const liveID = "00000000-0000-4000-8000-000000000202"
	liveProcess := exec.Command("sleep", "30")
	if err := liveProcess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = liveProcess.Process.Kill()
		_ = liveProcess.Wait()
	})
	livePID := liveProcess.Process.Pid
	liveSocket := filepath.Join(root, "run", strconv.Itoa(livePID)+".sock")
	liveListener, err := net.Listen("unix", liveSocket)
	if err != nil {
		t.Fatal(err)
	}
	liveRow := claudeNativePeerRecord{
		PID: livePID, SessionID: liveID, ProcStart: federator.ProcessStart(livePID),
		Entrypoint: "cli", Kind: "interactive", MessagingSocketPath: liveSocket,
	}
	liveBody, err := json.Marshal(liveRow)
	if err != nil {
		t.Fatal(err)
	}
	liveRowPath := filepath.Join(publicConfig, "sessions", strconv.Itoa(livePID)+".json")
	if err := os.WriteFile(liveRowPath, liveBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunClaudePeer([]string{"--session-id", liveID, "--group", "must-not-persist"}); err == nil {
		t.Fatal("live ordinary Claude attachment unexpectedly launched as a peer")
	}
	assertCatalogAbsent(liveID)
	_ = liveListener.Close()
	if err := os.Remove(liveRowPath); err != nil {
		t.Fatal(err)
	}

	const invalidSettingsID = "00000000-0000-4000-8000-000000000203"
	if err := RunClaudePeer([]string{
		"--session-id", invalidSettingsID, "--group", "must-not-persist", "--settings", "{",
	}); err == nil {
		t.Fatal("invalid Claude settings unexpectedly launched")
	}
	assertCatalogAbsent(invalidSettingsID)

	const startupFailureID = "00000000-0000-4000-8000-000000000204"
	t.Setenv(claudePeerNativeFailBeforeRowEnv, "1")
	if err := RunClaudePeer([]string{
		"--session-id", startupFailureID, "--group", "must-roll-back", "--dangerously-skip-permissions",
	}); err == nil {
		t.Fatal("Claude native startup failure unexpectedly succeeded")
	}
	t.Setenv(claudePeerNativeFailBeforeRowEnv, "")
	assertCatalogAbsent(startupFailureID)
	failedLifecycleRoot := claudePeerLifecycleRoot(stateDir, "host-test", startupFailureID)
	if entries, err := os.ReadDir(failedLifecycleRoot); err != nil || len(entries) != 0 {
		t.Fatalf("failed Claude startup retained lifecycle residue: entries=%v err=%v", entries, err)
	}
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
	if err := RunClaudePeer([]string{"--resume", plan.sessionID, "--", "prompt text"}); err != nil {
		t.Fatalf("resume with durable yolo and prompt delimiter: %v", err)
	}
	lifecycleRoot := claudePeerLifecycleRoot(stateDir, "host-test", plan.sessionID)
	attachmentLock, err := acquireClaudePeerProfileLock(lifecycleRoot)
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
	entries, err := os.ReadDir(filepath.Join(publicConfig, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	serviceRows := 0
	serviceKeys := 0
	nativeRows := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".key") {
			body, readErr := os.ReadFile(filepath.Join(publicConfig, "sessions", entry.Name()))
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
		body, _ := os.ReadFile(filepath.Join(publicConfig, "sessions", entry.Name()))
		var row map[string]any
		_ = json.Unmarshal(body, &row)
		if service, _ := row["agentService"].(bool); service {
			serviceRows++
		} else {
			nativeRows++
		}
	}
	if serviceRows != 1 || serviceKeys != 1 || nativeRows != 0 {
		t.Fatalf("shared Claude registry has service=%d keys=%d native=%d; entries=%v", serviceRows, serviceKeys, nativeRows, entries)
	}
	ordinaryID := "00000000-0000-4000-8000-000000000123"
	if err := RunClaudePeer([]string{"--resume", ordinaryID, "--peer-name", "adopted"}); err != nil {
		t.Fatalf("adopt ordinary Claude session as peer: %v", err)
	}
	adopted, err := federator.LookupSessionPreferences(runtimeDir, ordinaryID)
	if err != nil || adopted.Preference.Product != "claude" || adopted.Preference.Kind != "interactive" {
		t.Fatalf("adopted Claude catalog row = %+v, %v", adopted, err)
	}
	if lifecycleEntries, err := os.ReadDir(lifecycleRoot); err != nil || len(lifecycleEntries) != 0 {
		t.Fatalf("Agent Sessions lifecycle root retained native/settings artifacts: %v, %v", lifecycleEntries, err)
	}
	status, err := federator.ReadAgentStatus(runtimeDir)
	if err != nil || status.LocalPeers != 0 {
		t.Fatalf("Claude peer was not unregistered: %+v, %v", status, err)
	}
	const cleanupFailureID = "00000000-0000-4000-8000-000000000210"
	t.Setenv(claudePeerNativeFailCleanupEnv, "1")
	cleanupFailure := RunClaudePeer([]string{"--session-id", cleanupFailureID, "--peer-name", "cleanup-debt"})
	t.Setenv(claudePeerNativeFailCleanupEnv, "")
	if cleanupFailure == nil || !strings.Contains(cleanupFailure.Error(), "socket path changed type") {
		t.Fatalf("changed native socket cleanup = %v", cleanupFailure)
	}
	preparationEntries, err := os.ReadDir(filepath.Join(stateDir, "claude-peer-preparations"))
	if err != nil || len(preparationEntries) != 1 {
		t.Fatalf("failed cleanup did not retain one durable preparation: entries=%v err=%v", preparationEntries, err)
	}
	if preference, err := federator.LookupSessionPreferences(runtimeDir, cleanupFailureID); err != nil || preference.Preference.Product != "claude" {
		t.Fatalf("committed cleanup debt lost catalog: %+v err=%v", preference, err)
	}
	const settingsCleanupFailureID = "00000000-0000-4000-8000-000000000211"
	settingsFailureRoot := claudePeerLifecycleRoot(stateDir, "host-test", settingsCleanupFailureID)
	settingsFailurePath := filepath.Join(settingsFailureRoot, "launch-settings.json")
	t.Setenv(claudePeerNativeFailSettingsCleanupEnv, settingsFailurePath)
	settingsCleanupFailure := RunClaudePeer([]string{
		"--session-id", settingsCleanupFailureID, "--peer-name", "settings-cleanup-debt",
	})
	t.Setenv(claudePeerNativeFailSettingsCleanupEnv, "")
	if settingsCleanupFailure == nil || !strings.Contains(settingsCleanupFailure.Error(), "launch settings changed type") {
		t.Fatalf("changed launch-settings cleanup = %v", settingsCleanupFailure)
	}
	preparationEntries, err = os.ReadDir(filepath.Join(stateDir, "claude-peer-preparations"))
	if err != nil || len(preparationEntries) != 2 {
		t.Fatalf("settings cleanup did not retain a second durable preparation: entries=%v err=%v", preparationEntries, err)
	}
	if preference, err := federator.LookupSessionPreferences(runtimeDir, settingsCleanupFailureID); err != nil || preference.Preference.Product != "claude" {
		t.Fatalf("settings cleanup debt lost committed catalog: %+v err=%v", preference, err)
	}
	if info, err := os.Lstat(settingsFailurePath); err != nil || !info.IsDir() {
		t.Fatalf("changed launch-settings artifact was not preserved for retry: info=%v err=%v", info, err)
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
	if err := cleanupClaudePeerNativeArtifacts(root, row, row.ProcStart, "", row.SessionID, nil, nil); err == nil {
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
	other, err := acquireClaudePeerProfileLock(filepath.Join(shortClaudePeerTestRoot(t), "other"))
	if err != nil {
		t.Fatalf("unrelated Claude session was serialized by another lock: %v", err)
	}
	releaseClaudePeerProfileLock(other)
}

func TestPrepareClaudePeerAttachmentPreservesUnrelatedSharedRows(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	directory := filepath.Join(root, "sessions")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	ordinary := filepath.Join(directory, strconv.Itoa(os.Getpid())+".json")
	service := filepath.Join(directory, "999999999.json")
	if err := os.WriteFile(ordinary, []byte(`{"pid":1,"sessionId":"ordinary-session"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service, []byte(`{"pid":999999999,"sessionId":"agent","agentService":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudePeerAttachment(root, "managed-session"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{ordinary, service} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrelated shared row was changed: %s: %v", path, err)
		}
	}
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
	if err := cleanupClaudePeerNativeArtifacts(root, claudeNativePeerRecord{PID: pid}, row.ProcStart, "", row.SessionID, nil, nil); err != nil {
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
	if err := cleanupClaudePeerNativeArtifacts(root, claudeNativePeerRecord{PID: pid}, row.ProcStart, "", row.SessionID, nil, nil); err != nil {
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
	validName, err := federator.ClaudeServiceKeyName(pid, socket)
	if err != nil {
		t.Fatal(err)
	}
	validKey := filepath.Join(directory, validName)
	invalidKey := filepath.Join(directory, strconv.Itoa(pid)+".not-a-token.key")
	for path, body := range map[string][]byte{record: body, validKey: []byte(`{"peerToken":"secret"}`), invalidKey: []byte("keep")} {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupClaudePeerNativeArtifacts(root, row, row.ProcStart, "", row.SessionID, nil, nil); err != nil {
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

func TestCleanupClaudePeerArtifactsRemovesOnlyNewTokenBeforeNativeRow(t *testing.T) {
	root := shortClaudePeerTestRoot(t)
	directory := filepath.Join(root, "sessions")
	if err := os.MkdirAll(directory, 0700); err != nil {
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
	identity := procinfo.Read(pid)
	if identity.Status != procinfo.Known || identity.Start == "" || identity.StrongStart == "" {
		t.Fatal("test adapter has no exact process identity")
	}
	expectedStart := identity.Start
	preexistingName := strconv.Itoa(pid) + "." + strings.Repeat("b", 64) + ".key"
	replacedName := strconv.Itoa(pid) + "." + strings.Repeat("a", 64) + ".key"
	malformedName := strconv.Itoa(pid) + ".not-native.key"
	for name, body := range map[string]string{
		preexistingName: `{"peerToken":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","procStart":"older"}`,
		replacedName:    `{"peerToken":"dddddddddddddddddddddddddddddddd","procStart":"older"}`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := federator.ClaudePeerKeySidecars(root, pid)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		replacedName:  `{"peerToken":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","procStart":"` + expectedStart + `"}`,
		malformedName: `{"peerToken":"cccccccccccccccccccccccccccccccc","procStart":"` + expectedStart + `"}`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	observed, err := federator.ObserveClaudePeerNewKeySidecars(
		root, pid, expectedStart, identity.StrongStart, baseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = adapter.Wait()
	if err := cleanupClaudePeerNativeArtifacts(
		root,
		claudeNativePeerRecord{PID: pid},
		expectedStart,
		identity.StrongStart,
		"token-only",
		baseline,
		observed,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(directory, replacedName)); !os.IsNotExist(err) {
		t.Fatalf("launch-owned token-only sidecar survived cleanup: %v", err)
	}
	// A same-PID key that appears only after the observed adapter has exited is
	// ambiguous on Darwin's second-resolution native procStart. Preserve it
	// unless it was fingerprinted while the exact strong adapter was live.
	if err := os.WriteFile(
		filepath.Join(directory, replacedName),
		[]byte(`{"peerToken":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","procStart":"`+expectedStart+`"}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := cleanupClaudePeerNativeArtifacts(
		root, claudeNativePeerRecord{PID: pid}, expectedStart, identity.StrongStart,
		"token-only", baseline, nil,
	); err == nil {
		t.Fatal("unobserved token-only sidecar was removed after PID reuse")
	}
	if _, err := os.Lstat(filepath.Join(directory, replacedName)); err != nil {
		t.Fatalf("ambiguous post-exit sidecar was not preserved: %v", err)
	}
	for _, name := range []string{preexistingName, malformedName} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("unowned sidecar %s was removed: %v", name, err)
		}
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
