package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/qwenprofile"
)

const testQwenSessionID = "12345678-1234-4234-8234-123456789abc"

func TestQwenPeerManagedArgumentContract(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	profile := filepath.Join(root, "profiles", "qwen")
	runtimeDir := filepath.Join(root, "runtime")
	for _, path := range []string{home, profile, runtimeDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lookup := qwenTestLookup(map[string]string{"HOME": home, "QWEN_RUNTIME_DIR": runtimeDir})
	tests := []struct {
		name       string
		args       []string
		wantMode   qwenPeerMode
		wantName   string
		wantTarget string
		wantPref   qwenLaunchPreference
		wantNative []string
		wantGroups []string
		wantHome   string
	}{
		{
			name: "fresh name and groups", args: []string{"-n", "reviewer", "-g", "project", "--group=review", "--model", "qwen3-coder"},
			wantMode: qwenPeerModeFresh, wantName: "reviewer", wantPref: qwenLaunchNativeDefault,
			wantNative: []string{"--model", "qwen3-coder"}, wantGroups: []string{"project", "review"},
		},
		{
			name: "wrapper yolo", args: []string{"--yolo", "--theme", "dark"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchYolo,
			wantNative: []string{"--yolo", "--theme", "dark"},
		},
		{
			name: "wrapper no yolo", args: []string{"--no-yolo", "--theme=dark"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchNonYolo,
			wantNative: []string{"--approval-mode", "default", "--theme=dark"},
		},
		{
			name: "native approval mode", args: []string{"--approval-mode", "plan", "--model", "qwen3-coder"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchPreference("native:plan"),
			wantNative: []string{"--approval-mode", "plan", "--model", "qwen3-coder"},
		},
		{
			name: "exact resume", args: []string{"--resume", testQwenSessionID, "--model", "qwen3-coder"},
			wantMode: qwenPeerModeResume, wantTarget: testQwenSessionID, wantPref: qwenLaunchNativeDefault,
			wantNative: []string{"--resume", testQwenSessionID, "--model", "qwen3-coder"},
		},
		{
			name: "name remains product selector", args: []string{"--resume=reviewer"},
			wantMode: qwenPeerModeResume, wantTarget: "reviewer", wantPref: qwenLaunchNativeDefault,
			wantNative: []string{"--resume=reviewer"},
		},
		{
			name: "bare resume remains product selector", args: []string{"--resume"},
			wantMode: qwenPeerModeResume, wantPref: qwenLaunchNativeDefault,
			wantNative: []string{"--resume"},
		},
		{
			name: "explicit profile", args: []string{"--qwen-home", profile},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchNativeDefault, wantHome: qwenTestCanonicalPath(t, profile),
		},
		{
			name: "prompt boundary untouched", args: []string{"--no-yolo", "--", "prompt", "--approval-mode", "yolo", "-g", "data"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchNonYolo,
			wantNative: []string{"--approval-mode", "default", "--", "prompt", "--approval-mode", "yolo", "-g", "data"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := parseQwenPeerArgs(test.args, root, lookup)
			if err != nil {
				t.Fatal(err)
			}
			if plan.mode != test.wantMode || plan.peerName != test.wantName || plan.resumeTarget != test.wantTarget || plan.launchPreference != test.wantPref {
				t.Fatalf("plan = %+v", plan)
			}
			if !slices.Equal(plan.nativeArgs, test.wantNative) || !reflect.DeepEqual(plan.peerContext.groups, test.wantGroups) {
				t.Fatalf("native/groups = %q/%q, want %q/%q", plan.nativeArgs, plan.peerContext.groups, test.wantNative, test.wantGroups)
			}
			if test.wantHome != "" && (!plan.profile.QwenHomeSet || plan.profile.QwenHome != test.wantHome) {
				t.Fatalf("profile = %+v, want home %q", plan.profile, test.wantHome)
			}
		})
	}
}

func TestQwenPeerRejectsConflictingOrInexactWrapperArguments(t *testing.T) {
	root := t.TempDir()
	lookup := qwenTestLookup(map[string]string{"HOME": root})
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "repeated yolo", args: []string{"--yolo", "--yolo"}, want: "permission"},
		{name: "contradictory wrapper", args: []string{"--yolo", "--no-yolo"}, want: "conflict"},
		{name: "wrapper then native", args: []string{"--yolo", "--approval-mode", "yolo"}, want: "conflict"},
		{name: "continue", args: []string{"--continue"}, want: "managed resume"},
		{name: "resume repeated", args: []string{"--resume", testQwenSessionID, "--resume=other"}, want: "more than once"},
		{name: "session id caller controlled", args: []string{"--session-id", testQwenSessionID}, want: "managed"},
		{name: "relative profile", args: []string{"--qwen-home", "relative"}, want: "absolute"},
		{name: "empty name", args: []string{"--name="}, want: "non-empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseQwenPeerArgs(test.args, root, lookup)
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("parse error = %#v, want exit 2 containing %q", err, test.want)
			}
		})
	}
}

func TestQwenPeerHelpAdvertisesManagedOptions(t *testing.T) {
	help := qwenPeerUsage()
	for _, option := range []string{"--name", "--group", "--yolo", "--no-yolo", "--qwen-home", "--resume", "--"} {
		if !strings.Contains(help, option) {
			t.Errorf("Qwen peer help omits %s", option)
		}
	}
}

func TestRunQwenPeerPassesNativeNameAndOwnsLaunchFiles(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	home := filepath.Join(root, "qwen")
	for _, path := range []string{cwd, home} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	qwenTestChdir(t, cwd)
	executable := filepath.Join(root, "qwen-bin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_PEER_QWEN_BIN", executable)
	t.Setenv("HOME", root)
	executed := false
	launchRoot := ""
	err := runQwenPeer(context.Background(), []string{"--qwen-home", home, "--resume", "Reviewer", "-g", "project"},
		qwenPeerDependencies{
			exec: func(string, []string, []string) error { return errors.New("unexpected exec") },
			run: func(_ context.Context, launch QwenNativeLaunch) error {
				executed = true
				if launch.Executable != executable || !slices.Contains(launch.Arguments, "Reviewer") || slices.Contains(launch.Arguments, testQwenSessionID) {
					t.Fatalf("native exec = %q %q", launch.Executable, launch.Arguments)
				}
				if value, ok := qwenTestEnvironment(launch.Environment, peerSessionIDEnv); ok {
					t.Fatalf("unknown resume identity was exported as %q", value)
				}
				input, ok := qwenTestEnvironment(launch.Environment, QwenInputFileEnv)
				if !ok || !filepath.IsAbs(input) {
					t.Fatalf("native input environment = %q/%v", input, ok)
				}
				launchRoot = filepath.Dir(input)
				if _, err := os.Stat(input); err != nil {
					t.Fatalf("launcher-owned input is unavailable: %v", err)
				}
				events := qwenTestArg(launch.Arguments, "--json-file")
				if filepath.Dir(events) != launchRoot {
					t.Fatalf("launch files have different owners: %q / %q", input, events)
				}
				if eventEnv, ok := qwenTestEnvironment(launch.Environment, QwenEventsFileEnv); !ok || eventEnv != events {
					t.Fatalf("native events environment = %q/%v, want %q", eventEnv, ok, events)
				}
				if launch.InputPath != input || launch.EventsPath != events || launch.QwenHome != home || launch.Cwd != cwd {
					t.Fatalf("native launch = %+v", launch)
				}
				return nil
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if !executed || launchRoot == "" {
		t.Fatal("Qwen child was not run")
	}
	if _, err := os.Stat(launchRoot); !os.IsNotExist(err) {
		t.Fatalf("launcher-owned directory survived child exit: %v", err)
	}
}

func TestRunQwenPeerExecutesOnlyTheNativeLaunchWithoutAProductProbe(t *testing.T) {
	root := t.TempDir()
	qwenTestChdir(t, root)
	executable := filepath.Join(root, "qwen")
	invocations := filepath.Join(root, "invocations")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' launch >>\"$QWEN_INVOCATIONS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_PEER_QWEN_BIN", executable)
	t.Setenv("QWEN_INVOCATIONS", invocations)
	err := runQwenPeer(context.Background(), nil,
		qwenPeerDependencies{
			exec: func(string, []string, []string) error { return errors.New("unexpected exec") },
			run: func(_ context.Context, launch QwenNativeLaunch) error {
				command := exec.Command(launch.Executable, launch.Arguments...) //nolint:gosec // test-owned executable.
				command.Env = launch.Environment
				return command.Run()
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "launch\n" {
		t.Fatalf("Qwen executable invocations = %q, want one native launch", body)
	}
}

func TestQwenLaunchDoesNotCollideWithStaleSessionScopedArtifacts(t *testing.T) {
	root := t.TempDir()
	qwenTestChdir(t, root)
	executable := filepath.Join(root, "qwen")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_PEER_QWEN_BIN", executable)
	stale := filepath.Join(root, "state", "native", "qwen", testQwenSessionID)
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	runs := 0
	err := runQwenPeer(context.Background(), []string{"--state-dir", filepath.Join(root, "state"), "--resume", testQwenSessionID}, qwenPeerDependencies{
		exec: func(string, []string, []string) error { return errors.New("unexpected exec") },
		run: func(_ context.Context, launch QwenNativeLaunch) error {
			runs++
			if launchRoot := filepath.Dir(qwenTestArg(launch.Arguments, "--input-file")); launchRoot == stale {
				t.Fatal("launch reused the stale session-scoped directory")
			}
			return nil
		},
	})
	if err != nil || runs != 1 {
		t.Fatalf("resume with stale artifacts = %v, runs=%d", err, runs)
	}
}

func TestSubmitQwenNativeNameWaitsForExactProductRegistryRow(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input.jsonl")
	if err := os.WriteFile(input, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitAndSubmitQwenNativeName(ctx, home, testQwenSessionID, input, "product owned name")
	}()
	if err := os.WriteFile(filepath.Join(sessions, "other.json"), []byte(`{"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * qwenRegistryInterval)
	if body, err := os.ReadFile(input); err != nil || len(body) != 0 {
		t.Fatalf("unrelated row submitted a name: %q, %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "native.json"), []byte(`{"sessionId":"`+testQwenSessionID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("exact product registry row did not release name submission")
	}
	cancel()
	file, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var command struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if scanner := bufio.NewScanner(file); !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &command) != nil {
		t.Fatal("Qwen native name command was not framed")
	}
	if command.Type != "submit" || command.Text != "/rename product owned name" {
		t.Fatalf("Qwen native name command = %+v", command)
	}
}

func qwenTestArg(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func qwenTestCanonicalPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func qwenTestChdir(t *testing.T, path string) {
	t.Helper()
	prior, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prior); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

func qwenTestEnvironment(environment []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func qwenTestLookup(values map[string]string) qwenprofile.LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
