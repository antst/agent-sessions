package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
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
	lookup := qwenTestLookup(map[string]string{
		"HOME": home, "QWEN_RUNTIME_DIR": runtimeDir,
	})
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
			name:     "fresh native default with name and groups",
			args:     []string{"-n", "reviewer", "-g", "project", "--group=review", "--model", "qwen3-coder"},
			wantMode: qwenPeerModeFresh, wantName: "reviewer", wantPref: qwenLaunchNativeDefault,
			wantNative: []string{"--model", "qwen3-coder"}, wantGroups: []string{"project", "review"},
		},
		{
			name:     "wrapper yolo",
			args:     []string{"--yolo", "--theme", "dark"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchYolo,
			wantNative: []string{"--approval-mode", "yolo", "--theme", "dark"},
		},
		{
			name:     "wrapper no yolo",
			args:     []string{"--no-yolo", "--theme=dark"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchNonYolo,
			wantNative: []string{"--approval-mode", "default", "--theme=dark"},
		},
		{
			name:     "native approval mode",
			args:     []string{"--approval-mode", "plan", "--model", "qwen3-coder"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchPreference("native:plan"),
			wantNative: []string{"--approval-mode", "plan", "--model", "qwen3-coder"},
		},
		{
			name:     "native approval equals wrapper vocabulary",
			args:     []string{"--approval-mode=yolo"},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchPreference("native:yolo"),
			wantNative: []string{"--approval-mode=yolo"},
		},
		{
			name:     "exact resume",
			args:     []string{"--resume", testQwenSessionID, "--model", "qwen3-coder"},
			wantMode: qwenPeerModeResume, wantTarget: testQwenSessionID, wantPref: qwenLaunchNativeDefault,
			wantNative: []string{"--resume", testQwenSessionID, "--model", "qwen3-coder"},
		},
		{
			name:     "unique name resume remains a selector until catalog resolution",
			args:     []string{"--resume=reviewer"},
			wantMode: qwenPeerModeResume, wantTarget: "reviewer", wantPref: qwenLaunchNativeDefault,
			wantNative: []string{"--resume=reviewer"},
		},
		{
			name:     "explicit profile",
			args:     []string{"--qwen-home", profile},
			wantMode: qwenPeerModeFresh, wantPref: qwenLaunchNativeDefault, wantHome: profile,
		},
		{
			name:     "prompt boundary untouched",
			args:     []string{"--no-yolo", "--", "prompt", "--approval-mode", "yolo", "-g", "data"},
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
			if plan.mode != test.wantMode || plan.peerName != test.wantName || plan.resumeTarget != test.wantTarget {
				t.Fatalf("plan identity = %+v, want mode=%s name=%q target=%q", plan, test.wantMode, test.wantName, test.wantTarget)
			}
			if plan.launchPreference != test.wantPref {
				t.Fatalf("launch preference = %q, want %q", plan.launchPreference, test.wantPref)
			}
			if !slices.Equal(plan.nativeArgs, test.wantNative) {
				t.Fatalf("native argv = %q, want %q", plan.nativeArgs, test.wantNative)
			}
			if !reflect.DeepEqual(plan.peerContext.groups, test.wantGroups) {
				t.Fatalf("groups = %q, want %q", plan.peerContext.groups, test.wantGroups)
			}
			if test.wantHome != "" {
				if !plan.profile.QwenHomeSet || plan.profile.QwenHome != test.wantHome {
					t.Fatalf("profile = %+v, want QWEN_HOME=%q", plan.profile, test.wantHome)
				}
			} else if plan.profile.QwenHomeSet {
				t.Fatalf("default profile unexpectedly sets QWEN_HOME: %+v", plan.profile)
			}
			if plan.profile.QwenRuntimeDir != runtimeDir || !plan.profile.QwenRuntimeSet {
				t.Fatalf("runtime profile identity = %+v", plan.profile)
			}
		})
	}
}

func TestQwenPeerRejectsPermissionAndIdentityConflictsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	lookup := qwenTestLookup(map[string]string{"HOME": root})
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "repeated yolo", args: []string{"--yolo", "--yolo"}, want: "permission"},
		{name: "repeated no yolo", args: []string{"--no-yolo", "--no-yolo"}, want: "permission"},
		{name: "contradictory wrapper", args: []string{"--yolo", "--no-yolo"}, want: "conflict"},
		{name: "wrapper then native", args: []string{"--yolo", "--approval-mode", "yolo"}, want: "conflict"},
		{name: "native then wrapper", args: []string{"--approval-mode=default", "--no-yolo"}, want: "conflict"},
		{name: "repeated native", args: []string{"--approval-mode", "plan", "--approval-mode=yolo"}, want: "more than once"},
		{name: "native missing mode", args: []string{"--approval-mode"}, want: "requires"},
		{name: "continue", args: []string{"--continue"}, want: "exact"},
		{name: "fork", args: []string{"--fork-session", testQwenSessionID}, want: "not owner-attested"},
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

func TestQwenPeerHelpAdvertisesEveryManagedOption(t *testing.T) {
	help := qwenPeerUsage()
	for _, option := range []string{
		"-n", "--name", "-g", "--group", "--inherit-groups", "--no-inherit-groups",
		"--yolo", "--no-yolo", "--approval-mode", "--qwen-home", "--resume",
		"--runtime-dir", "--state-dir", "--",
	} {
		if !strings.Contains(help, option) {
			t.Errorf("Qwen peer help omits %s", option)
		}
	}
}

func TestQwenPeerProfileOverridePreservesRuntimePresence(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	profile := filepath.Join(root, "profile")
	for _, path := range []string{home, profile} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := parseQwenPeerArgs([]string{"--qwen-home=" + profile}, root, qwenTestLookup(map[string]string{
		"HOME": home,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.profile.QwenHomeSet || plan.profile.QwenHome != profile || plan.profile.QwenRuntimeSet {
		t.Fatalf("profile identity = %+v", plan.profile)
	}
	environment := qwenprofile.ApplyEnvironment([]string{"HOME=" + home, "QWEN_HOME=stale", "QWEN_RUNTIME_DIR=stale", "KEEP=yes"}, plan.profile)
	if !slices.Contains(environment, "QWEN_HOME="+profile) || slices.Contains(environment, "QWEN_RUNTIME_DIR=stale") || !slices.Contains(environment, "KEEP=yes") {
		t.Fatalf("Qwen environment = %q", environment)
	}
}

func TestQwenPeerResumeRequiresExactManagedIdentityAndProfile(t *testing.T) {
	root := t.TempDir()
	defaultProfile := qwenTestProfile(t, filepath.Join(root, "home"), "")
	otherProfile := qwenTestProfile(t, filepath.Join(root, "other-home"), "")
	record := qwenManagedResumeRecord{
		SessionID: testQwenSessionID, Name: "reviewer", Product: "qwen", Cwd: root,
		Profile: defaultProfile, LaunchPreference: qwenLaunchNonYolo,
	}
	for _, test := range []struct {
		name       string
		selector   string
		candidates []qwenManagedResumeRecord
		profile    qwenprofile.Identity
		want       string
	}{
		{name: "exact UUID", selector: testQwenSessionID, candidates: []qwenManagedResumeRecord{record}, profile: defaultProfile},
		{name: "unique durable name", selector: "reviewer", candidates: []qwenManagedResumeRecord{record}, profile: defaultProfile},
		{name: "missing", selector: "missing", profile: defaultProfile, want: "no managed Qwen session"},
		{name: "ambiguous", selector: "reviewer", candidates: []qwenManagedResumeRecord{record, qwenResumeRecordWithID(record, "22345678-1234-4234-8234-123456789abc")}, profile: defaultProfile, want: "ambiguous"},
		{name: "other product", selector: "reviewer", candidates: []qwenManagedResumeRecord{qwenResumeRecordWithProduct(record, "claude")}, profile: defaultProfile, want: "belongs to claude"},
		{name: "already live", selector: "reviewer", candidates: []qwenManagedResumeRecord{qwenResumeRecordLive(record)}, profile: defaultProfile, want: "already live"},
		{name: "profile mismatch", selector: "reviewer", candidates: []qwenManagedResumeRecord{record}, profile: otherProfile, want: "profile fingerprint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := qwenPeerPlan{
				mode: qwenPeerModeResume, resumeTarget: test.selector, profile: test.profile,
				launchPreference: qwenLaunchNativeDefault,
				nativeArgs:       []string{"--resume", test.selector, "--model", "qwen3-coder"},
			}
			resolved, err := resolveQwenManagedResume(plan, test.candidates)
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("resolve error = %v, want %q", err, test.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resolved.sessionID != testQwenSessionID || resolved.requestedCwd != root || resolved.launchPreference != qwenLaunchNonYolo {
				t.Fatalf("resolved resume = %+v", resolved)
			}
			wantArgs := []string{"--approval-mode", "default", "--resume", testQwenSessionID, "--model", "qwen3-coder"}
			if !slices.Equal(resolved.nativeArgs, wantArgs) {
				t.Fatalf("resume argv = %q, want %q", resolved.nativeArgs, wantArgs)
			}
		})
	}
}

func TestQwenPeerResumeExplicitPermissionOverridesDurableDefault(t *testing.T) {
	root := t.TempDir()
	profile := qwenTestProfile(t, filepath.Join(root, "home"), "")
	plan, err := parseQwenPeerArgs([]string{"--resume", testQwenSessionID, "--approval-mode", "plan"}, root, qwenTestLookup(map[string]string{
		"HOME": filepath.Join(root, "home"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveQwenManagedResume(plan, []qwenManagedResumeRecord{{
		SessionID: testQwenSessionID, Name: "reviewer", Product: "qwen", Cwd: root,
		Profile: profile, LaunchPreference: qwenLaunchYolo,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.launchPreference != qwenLaunchPreference("native:plan") ||
		!slices.Equal(resolved.nativeArgs, []string{"--resume", testQwenSessionID, "--approval-mode", "plan"}) {
		t.Fatalf("explicit resume permission was not retained: %+v", resolved)
	}
}

func TestQwenPeerPreparedLaunchRollsBackEveryPostReadinessStartupFailure(t *testing.T) {
	for _, failure := range []string{"native authentication failed after readiness", "native session_start mismatch", "native startup failed"} {
		t.Run(failure, func(t *testing.T) {
			calls := []string{}
			err := runQwenPreparedLaunch(qwenPreparedLaunchCallbacks{
				Prepare:             func() error { calls = append(calls, "prepare"); return nil },
				StartAndCorroborate: func() error { calls = append(calls, "start"); return errors.New(failure) },
				Commit:              func() error { calls = append(calls, "commit"); return nil },
				Rollback:            func() error { calls = append(calls, "rollback"); return nil },
			})
			if err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("launch error = %v", err)
			}
			if !slices.Equal(calls, []string{"prepare", "start", "rollback"}) {
				t.Fatalf("transaction calls = %v", calls)
			}
		})
	}
}

func TestRunQwenPeerReadinessPrecedesManagedMutation(t *testing.T) {
	root, runtimeDir, stateDir, executable := qwenLauncherTestAgent(t)
	t.Setenv("QWEN_PEER_QWEN_BIN", executable)
	t.Setenv("CODEX_PEER_NATIVE_RUNTIME", executable)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv(agentRuntimeDirEnv, runtimeDir)
	profile := filepath.Join(root, "qwen-home")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	execCalled := false
	err := runQwenPeer([]string{"--qwen-home", profile, "-n", "not-started"}, qwenPeerDependencies{
		readiness: func(_ context.Context, _ qwenreadiness.Request) (qwenreadiness.Report, error) {
			if _, err := os.Stat(filepath.Join(stateDir, "peer-lifecycles")); !os.IsNotExist(err) {
				t.Fatalf("managed lifecycle mutation preceded readiness: %v", err)
			}
			return qwenreadiness.Report{Ready: false, IntegrationReady: true, Issues: []qwenreadiness.Issue{{Code: "auth", Message: "not ready"}}}, nil
		},
		exec: func(string, []string, []string) error { execCalled = true; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "not ready") || execCalled {
		t.Fatalf("pre-readiness launch result = %v, exec=%v", err, execCalled)
	}
}

func TestRunQwenPeerExecsPreparedHostAndRollsBackExecFailure(t *testing.T) {
	root, runtimeDir, stateDir, executable := qwenLauncherTestAgent(t)
	wantCwd, err := canonicalQwenCwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_PEER_QWEN_BIN", executable)
	t.Setenv("CODEX_PEER_NATIVE_RUNTIME", executable)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv(agentRuntimeDirEnv, runtimeDir)
	profile := filepath.Join(root, "qwen-home")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("exec sentinel")
	var prepared federator.PeerRegistration
	err = runQwenPeer([]string{"--qwen-home", profile, "-n", "managed-qwen", "-g", "project", "--no-yolo"}, qwenPeerDependencies{
		readiness: func(_ context.Context, request qwenreadiness.Request) (qwenreadiness.Report, error) {
			if request.Workspace != wantCwd || request.Profile.QwenHome != profile {
				t.Fatalf("readiness request = %#v", request)
			}
			return qwenreadiness.Report{Ready: true, Version: "0.21.15", IntegrationReady: true}, nil
		},
		exec: func(path string, args []string, environment []string) error {
			if path != executable || len(args) < 11 || args[0] != "qwen-host" {
				t.Fatalf("host exec = %q %q", path, args)
			}
			registrationJSON := ""
			for index := range args {
				if args[index] == "--registration-json" && index+1 < len(args) {
					registrationJSON = args[index+1]
				}
			}
			if json.Unmarshal([]byte(registrationJSON), &prepared) != nil || prepared.QwenPreparation == nil {
				t.Fatalf("prepared registration JSON = %q", registrationJSON)
			}
			record, lookupErr := federator.LookupManagedSession(runtimeDir, prepared.SessionID)
			if lookupErr != nil || record.Preference.Qwen == nil || record.Preference.Qwen.LaunchPreference != "non_yolo" ||
				!slices.Equal(record.Preference.ExplicitGroups, []string{"project"}) {
				t.Fatalf("prepared Qwen catalog = %#v, %v", record, lookupErr)
			}
			for _, artifact := range []federator.QwenArtifactAttestation{prepared.QwenPreparation.Input, prepared.QwenPreparation.Events} {
				info, statErr := os.Lstat(artifact.Path)
				if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
					t.Fatalf("prepared artifact %s = %v, %v", artifact.Path, info, statErr)
				}
			}
			if value, ok := qwenTestEnvironment(environment, qwenCapabilityEnv); !ok || value == "" {
				t.Fatal("prepared host has no raw Qwen MCP capability")
			}
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("exec failure = %v", err)
	}
	if _, lookupErr := federator.LookupManagedSession(runtimeDir, prepared.SessionID); lookupErr == nil {
		t.Fatal("failed Qwen exec retained adopted catalog state")
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "peer-lifecycles", "qwen")); statErr == nil {
		entries, _ := os.ReadDir(filepath.Join(stateDir, "peer-lifecycles", "qwen"))
		if len(entries) != 0 {
			t.Fatalf("failed Qwen exec retained lifecycle roots: %v", entries)
		}
	}
}

func qwenLauncherTestAgent(t *testing.T) (root, runtimeDir, stateDir, executable string) {
	t.Helper()
	root = t.TempDir()
	runtimeDir, stateDir = filepath.Join(root, "agent-runtime"), filepath.Join(root, "state")
	executable = filepath.Join(root, "agent-session-runtime")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- federator.RunAgent(ctx, federator.AgentOptions{
			HostID: "qwen-launch-test", HostName: "qwen-launch-test",
			ClaudeConfigDir: filepath.Join(root, "claude"), RuntimeDir: runtimeDir, StateDir: stateDir,
			ScanInterval: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0),
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("host agent: %v", err)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := federator.ReadAgentStatus(runtimeDir); err == nil {
			return root, runtimeDir, stateDir, executable
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Qwen launcher test agent did not become ready")
	return "", "", "", ""
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

func qwenTestProfile(t *testing.T, home, runtime string) qwenprofile.Identity {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"HOME": home}
	if runtime != "" {
		if err := os.MkdirAll(runtime, 0o700); err != nil {
			t.Fatal(err)
		}
		values["QWEN_RUNTIME_DIR"] = runtime
	}
	profile, err := qwenprofile.ResolveEnvironment(qwenTestLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func qwenResumeRecordWithID(record qwenManagedResumeRecord, sessionID string) qwenManagedResumeRecord {
	record.SessionID = sessionID
	return record
}

func qwenResumeRecordWithProduct(record qwenManagedResumeRecord, product string) qwenManagedResumeRecord {
	record.Product = product
	return record
}

func qwenResumeRecordLive(record qwenManagedResumeRecord) qwenManagedResumeRecord {
	record.Live = true
	return record
}

func qwenTestLookup(values map[string]string) qwenprofile.LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
