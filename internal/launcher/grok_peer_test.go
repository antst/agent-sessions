package launcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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
		wantTarget string
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
			wantMode: grokModeResume, wantNative: []string{"--always-approve", "--no-alt-screen"}, wantTarget: testGrokSessionID,
		},
		{
			name:     "resume compact before options",
			args:     []string{"-r" + testGrokSessionID, "--model", "grok-4.6", "prompt"},
			wantMode: grokModeResume, wantNative: []string{"--model", "grok-4.6", "prompt"}, wantTarget: testGrokSessionID,
		},
		{
			name:       "native title resume is late bound",
			args:       []string{"--resume", "test", "--always-approve"},
			wantMode:   grokModeResume,
			wantNative: []string{"--always-approve"},
			wantTarget: "test",
		},
		{
			name:       "bare native resume is late bound",
			args:       []string{"--resume", "--no-alt-screen"},
			wantMode:   grokModeResume,
			wantNative: []string{"--no-alt-screen"},
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
			if plan.resumeTarget != test.wantTarget {
				t.Fatalf("resume selection = target %q, want %q", plan.resumeTarget, test.wantTarget)
			}
			if plan.mode == grokModeFresh && !threadIDPattern.MatchString(plan.sessionID) {
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
	leaderSocket := filepath.Join(root, "leader.sock")
	got := grokInteractiveArguments(plan, leaderSocket)
	want := []string{
		"--leader", "--leader-socket", leaderSocket, "--sandbox", "off",
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
		{"--resume="},
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
		{"--resume"},
		{"--resume", "human title"},
		{"--resume=human-title"},
		{"-rhuman-title"},
	} {
		if _, err := parseGrokPeerArgs(args, root); err != nil {
			t.Fatalf("rejected %q: %v", args, err)
		}
	}
}

func TestGrokPeerTitleResumePreservesNativeSelectorAndHostContext(t *testing.T) {
	root := t.TempDir()
	plan, err := parseGrokPeerArgs([]string{"--resume", "test", "--yolo", "-g", "umka"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if plan.resumeTarget != "test" || plan.sessionID != "" {
		t.Fatalf("product-owned resume plan = %+v", plan)
	}
	if !reflect.DeepEqual(plan.peerContext.groups, []string{"umka"}) || !plan.peerContext.groupsSpecified {
		t.Fatalf("peer context = %+v", plan.peerContext)
	}
	managed := grokInteractiveArguments(plan, filepath.Join(root, "leader.sock"))
	if !slices.Contains(managed, "test") || slices.Contains(managed, plan.sessionID) {
		t.Fatalf("native title selector was rewritten: %q", managed)
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

func TestGrokPeerProjectsDescriptorOwnedYoloLiterally(t *testing.T) {
	plan, err := parseGrokPeerArgs([]string{"--yolo", "--no-alt-screen"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.interactiveArgs, []string{"--yolo", "--no-alt-screen"}) {
		t.Fatalf("native arguments = %q", plan.interactiveArgs)
	}
	if plan.permissionMode != "bypassPermissions" {
		t.Fatalf("permission = %q", plan.permissionMode)
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
	canonicalRealDirectory, err := filepath.EvalSymlinks(realDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if plan.requestedCwd != canonicalRealDirectory || !plan.cwdExplicit {
		t.Fatalf("cwd = %q explicit=%v", plan.requestedCwd, plan.cwdExplicit)
	}
	if !reflect.DeepEqual(plan.interactiveArgs, args) {
		t.Fatalf("cwd inspection rewrote argv: %q", plan.interactiveArgs)
	}
}

func TestGrokPeerBuildsOneLauncherOwnedLeaderAndTUI(t *testing.T) {
	root := t.TempDir()
	grok := writeGrokFixture(t, root, validGrokHelpFixture())
	t.Setenv("GROK_PEER_GROK_BIN", grok)
	var got GrokNativeLaunch
	err := RunGrokPeer(context.Background(), []string{
		"--session-id", testGrokSessionID, "-n", "native-title", "-g", "project", "--no-alt-screen",
	}, func(_ context.Context, launch GrokNativeLaunch) error {
		got = launch
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Executable != grok || len(got.Groups) != 1 || got.Groups[0] != "project" || !filepath.IsAbs(got.LeaderSocket) {
		t.Fatalf("native launch = %+v", got)
	}
	if !slices.Contains(got.TUIArguments, "--session-id") || !slices.Contains(got.TUIArguments, testGrokSessionID) {
		t.Fatalf("tui=%q", got.TUIArguments)
	}
	if !reflect.DeepEqual(got.LeaderEnvironment, got.TUIEnvironment) ||
		environmentValue(got.LeaderEnvironment, peerSessionIDEnv) != testGrokSessionID ||
		environmentValue(got.LeaderEnvironment, peerProductEnv) != "grok" ||
		environmentValue(got.LeaderEnvironment, peerSessionNameEnv) != "native-title" ||
		environmentValue(got.LeaderEnvironment, peerGroupsEnv) != `["project"]` ||
		environmentValue(got.LeaderEnvironment, GrokLeaderSocketEnv) != got.LeaderSocket {
		t.Fatalf("leader and TUI launch context differ: leader=%q tui=%q", got.LeaderEnvironment, got.TUIEnvironment)
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

func TestGrokExecutableFallsBackToVendorCLIWhenPathContainsChatProduct(t *testing.T) {
	root := t.TempDir()
	chatDirectory := filepath.Join(root, "chat")
	home := filepath.Join(root, "home")
	chatMarker := filepath.Join(root, "chat-probed")
	writeGrokFixture(t, chatDirectory, `: >"$GROK_CHAT_MARKER"; printf '%s\n' 'AI coding agent powered by Grok' 'Commands:' '  daemon'`)
	want := writeGrokFixture(t, filepath.Join(home, ".grok", "bin"), validGrokHelpFixture())
	t.Setenv("HOME", home)
	t.Setenv("PATH", chatDirectory)
	t.Setenv("GROK_CHAT_MARKER", chatMarker)
	t.Setenv("GROK_PEER_GROK_BIN", "")

	got, err := grokExecutable()
	if err != nil || got != want {
		t.Fatalf("grok executable = %q, %v; want vendor fallback %q", got, err, want)
	}
	if _, err := os.Stat(chatMarker); err != nil {
		t.Fatalf("chat candidate was not contract-probed before fallback: %v", err)
	}
}

func TestGrokExecutableSelectsNewestValidatedManagedDownload(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	downloads := filepath.Join(home, ".grok", "downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	architecture := runtime.GOARCH
	switch architecture {
	case "amd64":
		architecture = "x86_64"
	case "arm64":
		architecture = "aarch64"
	}
	write := func(version string) string {
		t.Helper()
		path := filepath.Join(downloads, "grok-"+version+"-"+runtime.GOOS+"-"+architecture)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+validGrokHelpFixture()+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	_ = write("1.0.5")
	want := write("1.0.13")
	_ = write("1.0.9")
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	t.Setenv("GROK_PEER_GROK_BIN", "")

	got, err := grokExecutable()
	if err != nil || got != want {
		t.Fatalf("managed Grok executable = %q, %v; want newest %q", got, err, want)
	}
}

func TestGrokExecutableRejectsMacOSAppBundleBeforeProbe(t *testing.T) {
	for _, source := range []string{"override", "path", "fallback"} {
		for _, symlinked := range []bool{false, true} {
			name := source + "/direct"
			if symlinked {
				name = source + "/symlink"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				marker := filepath.Join(root, "gui-helper-invoked")
				bundleDirectory := filepath.Join(root, "Applications", "Grok.Build.APP", "cOnTeNtS", "MacOS")
				bundleCLI := writeGrokFixture(t, bundleDirectory, `: >"$GROK_GUI_MARKER"; `+validGrokHelpFixture())
				candidate := bundleCLI

				if symlinked {
					shimDirectory := filepath.Join(root, "shim")
					if err := os.MkdirAll(shimDirectory, 0o700); err != nil {
						t.Fatal(err)
					}
					candidate = filepath.Join(shimDirectory, "grok")
					if err := os.Symlink(bundleCLI, candidate); err != nil {
						t.Fatal(err)
					}
				}

				emptyPath := filepath.Join(root, "empty-path")
				if err := os.MkdirAll(emptyPath, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GROK_GUI_MARKER", marker)
				t.Setenv("GROK_PEER_GROK_BIN", "")
				t.Setenv("PATH", emptyPath)
				t.Setenv("HOME", filepath.Join(root, "home"))

				switch source {
				case "override":
					t.Setenv("GROK_PEER_GROK_BIN", candidate)
				case "path":
					t.Setenv("PATH", filepath.Dir(candidate))
				case "fallback":
					home := filepath.Join(root, "home")
					if !symlinked {
						home = filepath.Join(root, "Applications", "Fallback.APP", "CoNtEnTs", "Home")
						writeGrokFixture(t, filepath.Join(home, ".grok", "bin"), `: >"$GROK_GUI_MARKER"; `+validGrokHelpFixture())
					} else {
						fallback := filepath.Join(home, ".grok", "bin", "grok")
						if err := os.MkdirAll(filepath.Dir(fallback), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(bundleCLI, fallback); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv("HOME", home)
				}

				if got, err := grokExecutable(); err == nil {
					t.Fatalf("macOS application helper was accepted as Grok CLI: %q", got)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("macOS application helper was executed during validation: %v", err)
				}
			})
		}
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
