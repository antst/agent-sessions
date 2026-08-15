package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

const testGrokSessionID = "12345678-1234-4234-8234-123456789abc"

func TestGrokPeerManagedArgumentParity(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name       string
		args       []string
		wantMode   grokMode
		wantName   string
		wantNative []string
	}{
		{
			name:     "fresh strips only peer name",
			args:     []string{"--model", "grok-4.6", "-n", "reviewer", "--rules", "one", "two", "prompt"},
			wantMode: grokModeFresh, wantName: "reviewer",
			wantNative: []string{"--model", "grok-4.6", "--rules", "one", "two", "prompt"},
		},
		{
			name:     "fresh supplied id becomes managed prefix",
			args:     []string{"--session-id", testGrokSessionID, "--no-alt-screen"},
			wantMode: grokModeFresh, wantNative: []string{"--no-alt-screen"},
		},
		{
			name:     "native values that look like peer flags stay values",
			args:     []string{"--rules", "-n", "--system-prompt", "--peer-name", "-n", "actual-peer"},
			wantMode: grokModeFresh, wantName: "actual-peer",
			wantNative: []string{"--rules", "-n", "--system-prompt", "--peer-name"},
		},
		{
			name:     "resume after options",
			args:     []string{"--always-approve", "--resume", testGrokSessionID, "--no-alt-screen"},
			wantMode: grokModeResume, wantNative: []string{"--always-approve", "--no-alt-screen"},
		},
		{
			name:     "resume compact before options",
			args:     []string{"-r" + testGrokSessionID, "--model", "grok-4.6", "prompt"},
			wantMode: grokModeResume, wantNative: []string{"--model", "grok-4.6", "prompt"},
		},
		{
			name:     "bare boundary is untouched",
			args:     []string{"-n=boundary", "--", "--resume", "not-a-selector", "-n", "prompt-data"},
			wantMode: grokModeFresh, wantName: "boundary",
			wantNative: []string{"--", "--resume", "not-a-selector", "-n", "prompt-data"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := parseGrokPeerArgs(test.args, root)
			if err != nil {
				t.Fatal(err)
			}
			if plan.mode != test.wantMode || plan.peerName != test.wantName {
				t.Fatalf("plan identity = %+v, want mode=%s name=%q", plan, test.wantMode, test.wantName)
			}
			if !threadIDPattern.MatchString(plan.sessionID) {
				t.Fatalf("session id = %q", plan.sessionID)
			}
			if !reflect.DeepEqual(plan.interactiveArgs, test.wantNative) {
				t.Fatalf("forwarded argv = %q, want %q", plan.interactiveArgs, test.wantNative)
			}
		})
	}
}

func TestGrokPeerManagedPrefixPreservesCallerVector(t *testing.T) {
	root := t.TempDir()
	plan, err := parseGrokPeerArgs([]string{
		"--resume=" + testGrokSessionID,
		"--rules", "first", "second", "--prompt-json", `[{"type":"text","text":"hello"}]`,
		"--", "literal", "--leader-socket", "prompt-data",
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	ready := grokHostReady{LeaderSocket: filepath.Join(root, "leader.sock")}
	got := grokInteractiveArguments(plan, ready)
	want := []string{
		"--leader", "--leader-socket", ready.LeaderSocket, "--sandbox", "off",
		"--resume", testGrokSessionID,
		"--rules", "first", "second", "--prompt-json", `[{"type":"text","text":"hello"}]`,
		"--", "literal", "--leader-socket", "prompt-data",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managed argv = %q, want %q", got, want)
	}
}

func TestGrokPeerSubcommandsAndInformationalFlagsDoNotActivatePeer(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"--version"},
		{"-n", "ignored", "--help"},
		{"sessions", "list", "-n", "5"},
		{"agent", "stdio", "--leader-socket", "/native/socket"},
		{"version"},
		{"v"},
		{"disk-usage"},
	} {
		plan, err := parseGrokPeerArgs(args, root)
		if err != nil {
			t.Fatalf("parse %q: %v", args, err)
		}
		if plan.mode != grokModePassthrough {
			t.Fatalf("%q classified as %+v", args, plan)
		}
		if args[0] == "sessions" && !reflect.DeepEqual(plan.originalArgs, args) {
			t.Fatalf("subcommand -n was stolen: got %q want %q", plan.originalArgs, args)
		}
	}
	if _, err := parseGrokPeerArgs([]string{"-n", "peer", "sessions", "list"}, root); err == nil {
		t.Fatal("peer name was accepted for an administrative subcommand")
	}
}

func TestGrokPeerRejectsUnmanagedOrUnresolvableInteractiveModes(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"--leader"},
		{"--leader=true"},
		{"--no-leader"},
		{"--no-leader=true"},
		{"--leader-socket", "/tmp/user.sock"},
		{"--leader-socket=/tmp/user.sock"},
		{"--sandbox", "workspace-write"},
		{"--sandbox=danger-full-access"},
		{"--continue"},
		{"--continue=true"},
		{"-c"},
		{"--resume"},
		{"--resume", "human title"},
		{"--resume=human-title"},
		{"-rhuman-title"},
		{"--fork-session"},
		{"--session-id", testGrokSessionID, "--resume", testGrokSessionID},
	} {
		if _, err := parseGrokPeerArgs(args, root); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	for _, args := range [][]string{
		{"--sandbox", "off"},
		{"--sandbox=off"},
		{"--resume", testGrokSessionID},
		{"--load=" + testGrokSessionID},
	} {
		if _, err := parseGrokPeerArgs(args, root); err != nil {
			t.Fatalf("rejected %q: %v", args, err)
		}
	}
}

func TestGrokPeerPermissionPublicationUsesLastPolicyOption(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: nil, want: "default"},
		{args: []string{"--always-approve"}, want: "bypassPermissions"},
		{args: []string{"--yolo"}, want: "bypassPermissions"},
		{args: []string{"--permission-mode", "always-approve"}, want: "bypassPermissions"},
		{args: []string{"--permission-mode=bypassPermissions"}, want: "bypassPermissions"},
		{args: []string{"--always-approve", "--permission-mode", "default"}, want: "default"},
		{args: []string{"--permission-mode=always-approve", "--always-approve"}, want: "bypassPermissions"},
	} {
		plan, err := parseGrokPeerArgs(test.args, root)
		if err != nil {
			t.Fatalf("parse %q: %v", test.args, err)
		}
		if plan.permissionMode != test.want {
			t.Fatalf("permission for %q = %q, want %q", test.args, plan.permissionMode, test.want)
		}
		if !slices.Equal(plan.interactiveArgs, test.args) {
			t.Fatalf("permission inspection rewrote argv: got %q want %q", plan.interactiveArgs, test.args)
		}
	}
}

func TestGrokPeerCwdIsCanonicalButNativeArgvIsUntouched(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Fatal(err)
	}
	args := []string{"--cwd", alias, "--model", "grok-4.6"}
	plan, err := parseGrokPeerArgs(args, root)
	if err != nil {
		t.Fatal(err)
	}
	if plan.requestedCwd != realDirectory || !plan.cwdExplicit {
		t.Fatalf("cwd = %q explicit=%v", plan.requestedCwd, plan.cwdExplicit)
	}
	if !reflect.DeepEqual(plan.interactiveArgs, args) {
		t.Fatalf("cwd inspection rewrote argv: %q", plan.interactiveArgs)
	}
}

func TestGrokHostContractAndReadinessValidation(t *testing.T) {
	root := t.TempDir()
	request := grokHostRequest{
		SessionID: testGrokSessionID, Cwd: root, Name: "reviewer", OwnerPID: 42,
		OwnerProcStart: "start-token", LaunchToken: "secret-token",
		PermissionMode: "bypassPermissions", GrokBin: "/opt/grok/bin/grok",
	}
	got := grokHostArguments(request)
	want := []string{
		"grok-host", "--session-id", testGrokSessionID, "--cwd", root,
		"--owner-pid", "42", "--owner-proc-start", "start-token",
		"--permission-mode", "bypassPermissions", "--grok-bin", "/opt/grok/bin/grok",
		"--name", "reviewer",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("host args = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(got, " "), request.LaunchToken) {
		t.Fatal("launch token leaked into process argv")
	}
	line := `{"ready":true,"session_id":"` + testGrokSessionID + `","cwd":"` + root + `","leader_socket":"` + filepath.Join(root, "leader.sock") + `","control_socket":"` + filepath.Join(root, "control.sock") + `"}`
	ready, err := parseGrokHostReady(line, request)
	if err != nil || ready.SessionID != testGrokSessionID {
		t.Fatalf("readiness = %+v, %v", ready, err)
	}
	for _, bad := range []string{
		`{"ready":false,"session_id":"` + testGrokSessionID + `","cwd":"` + root + `","leader_socket":"/tmp/a","control_socket":"/tmp/b"}`,
		`{"ready":true,"session_id":"00000000-0000-4000-8000-000000000000","cwd":"` + root + `","leader_socket":"/tmp/a","control_socket":"/tmp/b"}`,
		`{"ready":true,"session_id":"` + testGrokSessionID + `","cwd":"` + root + `","leader_socket":"relative","control_socket":"/tmp/b"}`,
	} {
		if _, err := parseGrokHostReady(bad, request); err == nil {
			t.Fatalf("accepted readiness %s", bad)
		}
	}
}

func TestGrokLaunchEnvironmentReplacesInheritedAuthority(t *testing.T) {
	got := replaceGrokLaunchEnvironment([]string{
		"PATH=/bin", grokLaunchTokenEnv + "=stale", grokSessionIDEnv + "=stale",
		"GROK_PEER_NATIVE_RUNTIME=/stale/runtime", "KEEP=yes",
	}, "fresh-token", testGrokSessionID, "/exact/runtime")
	want := []string{
		"PATH=/bin", "KEEP=yes", grokLaunchTokenEnv + "=fresh-token", grokSessionIDEnv + "=" + testGrokSessionID,
		"GROK_PEER_NATIVE_RUNTIME=/exact/runtime",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %q, want %q", got, want)
	}
}

func TestGrokExecutableSkipsInvalidPathCandidateAndValidatesOverride(t *testing.T) {
	root := t.TempDir()
	invalidDirectory := filepath.Join(root, "invalid")
	validDirectory := filepath.Join(root, "valid")
	invalidMarker := filepath.Join(root, "invalid-called")
	writeGrokFixture(t, invalidDirectory, `: >"$INVALID_MARKER"; printf '%s\n' 'Desktop Grok helper'`)
	valid := writeGrokFixture(t, validDirectory, validGrokHelpFixture())
	t.Setenv("HOME", filepath.Join(root, "empty-home"))
	t.Setenv("PATH", invalidDirectory+string(os.PathListSeparator)+validDirectory)
	t.Setenv("INVALID_MARKER", invalidMarker)
	t.Setenv("GROK_PEER_GROK_BIN", "")
	got, err := grokExecutable()
	if err != nil || got != valid {
		t.Fatalf("grok executable = %q, %v; want %q", got, err, valid)
	}
	if _, err := os.Stat(invalidMarker); err != nil {
		t.Fatalf("invalid candidate was not actually probed: %v", err)
	}

	t.Setenv("GROK_PEER_GROK_BIN", filepath.Join(invalidDirectory, "grok"))
	_, err = grokExecutable()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 127 {
		t.Fatalf("invalid override error = %v", err)
	}
}

func TestGrokRuntimeSelectionDoesNotRequireCodex(t *testing.T) {
	root := t.TempDir()
	runtimePath := filepath.Join(root, "agent-session-runtime")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_PEER_NATIVE_RUNTIME", runtimePath)
	t.Setenv("CODEX_PEER_NATIVE_RUNTIME", filepath.Join(root, "must-not-win"))
	t.Setenv("CODEX_PEER_CODEX_BIN", filepath.Join(root, "absent-codex"))
	got, err := grokRuntimeExecutable()
	if err != nil || got != runtimePath {
		t.Fatalf("Grok runtime = %q, %v; want %q without Codex", got, err, runtimePath)
	}
}

func TestGrokRealCommandListHasNoPassthroughDrift(t *testing.T) {
	grok, err := grokExecutable()
	if err != nil {
		t.Skipf("real Grok CLI unavailable: %v", err)
	}
	output, err := exec.Command(grok, "--no-auto-update", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("grok --help: %v: %s", err, output)
	}
	commands := grokCommandsFromHelp(string(output))
	if len(commands) < 10 {
		t.Fatalf("extracted suspiciously few Grok commands: %q", commands)
	}
	for _, sentinel := range []string{"agent", "sessions", "worktree"} {
		if _, ok := commands[sentinel]; !ok {
			t.Fatalf("real command extraction missed sentinel %q: %v", sentinel, commands)
		}
	}
	root := t.TempDir()
	for command := range commands {
		plan, parseErr := parseGrokPeerArgs([]string{command}, root)
		if parseErr != nil || plan.mode != grokModePassthrough {
			t.Fatalf("real Grok command %q is not passthrough: %+v, %v", command, plan, parseErr)
		}
	}
}

func grokCommandsFromHelp(help string) map[string]struct{} {
	commands := make(map[string]struct{})
	inCommands := false
	for _, line := range strings.Split(help, "\n") {
		if strings.TrimSpace(line) == "Commands:" {
			inCommands = true
			continue
		}
		if !inCommands {
			continue
		}
		if strings.TrimSpace(line) == "" {
			if len(commands) > 0 {
				break
			}
			continue
		}
		if line == strings.TrimSpace(line) {
			break
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			commands[fields[0]] = struct{}{}
		}
	}
	return commands
}

func writeGrokFixture(t *testing.T, directory, body string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "grok")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func validGrokHelpFixture() string {
	markers := append([]string(nil), grokCLIHelpMarkers...)
	sort.Strings(markers)
	return "printf '%s\\n' " + strings.Join(quoteShellWords(markers), " ")
}

func quoteShellWords(words []string) []string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = "'" + strings.ReplaceAll(word, "'", "'\\''") + "'"
	}
	return quoted
}
