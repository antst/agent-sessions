package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
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
			wantNative: []string{"--approval-mode", "yolo", "--theme", "dark"},
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
		{name: "continue", args: []string{"--continue"}, want: "exact"},
		{name: "resume missing", args: []string{"--resume"}, want: "requires"},
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

func TestQwenProductSessionResolutionUsesNativeTranscriptsOnly(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "qwen")
	cwd := filepath.Join(root, "workspace")
	for _, path := range []string{home, cwd} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	profile := qwenTestProfile(t, home)
	qwenWriteTranscript(t, home, "one", testQwenSessionID, cwd, "Reviewer", "manual")

	if id, name, err := resolveQwenProductSession(profile, cwd, testQwenSessionID); err != nil || id != testQwenSessionID || name != "" {
		t.Fatalf("exact native id = %q/%q, %v", id, name, err)
	}
	if id, name, err := resolveQwenProductSession(profile, cwd, "reviewer"); err != nil || id != testQwenSessionID || name != "Reviewer" {
		t.Fatalf("product title = %q/%q, %v", id, name, err)
	}
	if _, _, err := resolveQwenProductSession(profile, cwd, "missing"); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("missing product title = %v", err)
	}

	qwenWriteTranscript(t, home, "two", "22345678-1234-4234-8234-123456789abc", cwd, "Reviewer", "manual")
	if _, _, err := resolveQwenProductSession(profile, cwd, "Reviewer"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous product title = %v", err)
	}
	qwenWriteTranscript(t, home, "three", "32345678-1234-4234-8234-123456789abc", cwd, "Generated", "auto")
	if _, _, err := resolveQwenProductSession(profile, cwd, "Generated"); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("automatic title became resume authority: %v", err)
	}
}

func TestRunQwenPeerResolvesNameThroughProductBeforeDaemonPreparation(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	home := filepath.Join(root, "qwen")
	for _, path := range []string{cwd, home} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	qwenTestChdir(t, cwd)
	qwenWriteTranscript(t, home, "project", testQwenSessionID, cwd, "Reviewer", "manual")
	executable := filepath.Join(root, "qwen-bin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_PEER_QWEN_BIN", executable)
	t.Setenv("HOME", root)
	input, events := filepath.Join(root, "input.jsonl"), filepath.Join(root, "events.jsonl")
	var prepared QwenDaemonPrepareRequest
	executed := false
	err := runQwenPeerWithDaemon(context.Background(), []string{"--qwen-home", home, "--resume", "Reviewer", "-g", "project"},
		func(_ context.Context, request QwenDaemonPrepareRequest) (QwenDaemonPrepareResult, error) {
			prepared = request
			return QwenDaemonPrepareResult{SessionID: request.ResumeTarget, InputPath: input, EventsPath: events, Capability: "live-only"}, nil
		}, qwenPeerDependencies{
			readiness: func(context.Context, qwenreadiness.Request) (qwenreadiness.Report, error) {
				return qwenreadiness.Report{Ready: true, IntegrationReady: true}, nil
			},
			exec: func(path string, args, environment []string) error {
				executed = true
				if path != executable || !slices.Contains(args, testQwenSessionID) || slices.Contains(args, "Reviewer") {
					t.Fatalf("native exec = %q %q", path, args)
				}
				if value, ok := qwenTestEnvironment(environment, peerSessionIDEnv); !ok || value != testQwenSessionID {
					t.Fatalf("native session environment = %q/%v", value, ok)
				}
				return nil
			},
			capture: func(int) (procinfo.Identity, error) { return procinfo.Identity{PID: 42}, nil },
		})
	if err != nil {
		t.Fatal(err)
	}
	if !executed || prepared.ResumeTarget != testQwenSessionID || prepared.SessionID != testQwenSessionID || prepared.Name != "Reviewer" || !slices.Equal(prepared.Groups, []string{"project"}) {
		t.Fatalf("prepared request = %+v, executed=%v", prepared, executed)
	}
}

func TestRunQwenPeerDoesNotPrepareBeforeReadiness(t *testing.T) {
	root := t.TempDir()
	qwenTestChdir(t, root)
	executable := filepath.Join(root, "qwen")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_PEER_QWEN_BIN", executable)
	prepared, executed := false, false
	err := runQwenPeerWithDaemon(context.Background(), nil,
		func(context.Context, QwenDaemonPrepareRequest) (QwenDaemonPrepareResult, error) {
			prepared = true
			return QwenDaemonPrepareResult{}, nil
		}, qwenPeerDependencies{
			readiness: func(context.Context, qwenreadiness.Request) (qwenreadiness.Report, error) {
				return qwenreadiness.Report{Ready: false, IntegrationReady: true, Issues: []qwenreadiness.Issue{{Code: "auth", Message: "not ready"}}}, nil
			},
			exec:    func(string, []string, []string) error { executed = true; return nil },
			capture: func(int) (procinfo.Identity, error) { return procinfo.Identity{PID: 42}, nil },
		})
	if err == nil || !strings.Contains(err.Error(), "not ready") || prepared || executed {
		t.Fatalf("readiness result = %v, prepared=%v executed=%v", err, prepared, executed)
	}
}

func qwenWriteTranscript(t *testing.T, home, project, id, cwd, title, source string) {
	t.Helper()
	directory := filepath.Join(home, "projects", project, "chats")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{
		{"sessionId": id, "cwd": cwd, "type": "system", "subtype": "session_start"},
		{"sessionId": id, "cwd": cwd, "type": "system", "subtype": "custom_title", "systemPayload": map[string]any{"customTitle": title, "titleSource": source}},
	}
	var body strings.Builder
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(encoded)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(directory, id+".jsonl"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
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

func qwenTestProfile(t *testing.T, home string) qwenprofile.Identity {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	profile, err := qwenprofile.ResolveEnvironment(qwenTestLookup(map[string]string{"HOME": t.TempDir(), "QWEN_HOME": home}))
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func qwenTestLookup(values map[string]string) qwenprofile.LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
