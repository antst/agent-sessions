package launcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

type codexPendingLaunchFixture struct {
	result  CodexDaemonPrepareResult
	awaited bool
	closed  bool
}

func (fixture *codexPendingLaunchFixture) Await() (CodexDaemonPrepareResult, error) {
	fixture.awaited = true
	return fixture.result, nil
}

func (fixture *codexPendingLaunchFixture) Close() error {
	fixture.closed = true
	return nil
}

func TestCodexPeerNativeArgumentParity(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		mode codexMode
		want []string
	}{
		{name: "fresh", args: []string{"-n", "reviewer", "--no-alt-screen"}, mode: modeFresh, want: []string{"--no-alt-screen"}},
		{name: "resume", args: []string{"resume", "reviewer", "--yolo"}, mode: modeResume, want: []string{"--yolo"}},
		{name: "global yolo resume", args: []string{"--yolo", "resume", "reviewer", "--no-alt-screen"}, mode: modeResume, want: []string{"--yolo", "--no-alt-screen"}},
		{name: "global display resume", args: []string{"--no-alt-screen", "resume", "reviewer"}, mode: modeResume, want: []string{"--no-alt-screen"}},
		{name: "prompt boundary", args: []string{"-C", realDirectory, "--", "-C", "prompt-data"}, mode: modeFresh, want: []string{"-C", realDirectory, "--", "-C", "prompt-data"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := parseCodexPeerArgs(test.args, root, "")
			if err != nil {
				t.Fatal(err)
			}
			if plan.mode != test.mode || !reflect.DeepEqual(plan.interactiveArgs, test.want) {
				t.Fatalf("plan = %+v, want mode=%s args=%q", plan, test.mode, test.want)
			}
			if test.mode == modeResume && plan.selectionTarget != "reviewer" {
				t.Fatalf("selection target = %q", plan.selectionTarget)
			}
			if test.mode == modeResume && slices.Contains(test.args, "--yolo") && !plan.requestedYolo {
				t.Fatalf("resume yolo was not mirrored into the prepared binding: %+v", plan)
			}
		})
	}
}

func TestCodexResumeSelectorPassesToProductVerbatim(t *testing.T) {
	root := t.TempDir()
	plan, err := projectCodexLaunchPlan([]string{"--resume", "shared", "-g", "current"}, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.mode != modeResume || plan.selectionTarget != "shared" || !reflect.DeepEqual(plan.peerContext.groups, []string{"current"}) {
		t.Fatalf("projected plan = %+v", plan)
	}
	plan, err = projectCodexLaunchPlan([]string{"--resume"}, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.mode != modeResume || plan.selectionTarget != "" {
		t.Fatalf("bare native resume projection = %+v", plan)
	}
}

func TestCodexPeerFreshAndResumeUseOnePendingNativeSelectionPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/true")
	remote := "unix://" + filepath.Join(home, "app-server-control", "app-server-control.sock")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		arguments    []string
		selectorKind string
		wantNative   []string
	}{
		{name: "fresh", arguments: []string{"-n", "native-name", "-g", "project"}, selectorKind: CodexLaunchSelectorFresh, wantNative: []string{"--remote", remote, "-C", workingDirectory}},
		{name: "resume name", arguments: []string{"--resume", "duplicate native name", "-g", "project"}, selectorKind: CodexLaunchSelectorName, wantNative: []string{"--remote", remote, "-C", workingDirectory, "resume", "duplicate native name"}},
		{name: "resume uuid", arguments: []string{"--resume", "00000000-0000-0000-0000-00000000c011"}, selectorKind: CodexLaunchSelectorID, wantNative: []string{"--remote", remote, "-C", workingDirectory, "resume", "00000000-0000-0000-0000-00000000c011"}},
		{name: "bare resume", arguments: []string{"--resume"}, selectorKind: CodexLaunchSelectorBare, wantNative: []string{"--remote", remote, "-C", workingDirectory, "resume"}},
		{name: "explicit cwd", arguments: []string{"-C", home, "--resume", "selected"}, selectorKind: CodexLaunchSelectorName, wantNative: []string{"--remote", remote, "resume", "selected", "-C", home}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sequence := []string{}
			var request CodexDaemonPrepareRequest
			pending := &codexPendingLaunchFixture{result: CodexDaemonPrepareResult{
				ThreadID: "00000000-0000-0000-0000-00000000c012", Name: "selected", Cwd: home,
			}}
			err := RunCodexPeerWithDaemon(context.Background(), test.arguments,
				func(_ context.Context, input CodexDaemonPrepareRequest) (CodexPendingLaunch, error) {
					sequence = append(sequence, "pending")
					request = input
					return pending, nil
				}, func(_ context.Context, launch CodexNativeLaunch) error {
					sequence = append(sequence, "native")
					if !reflect.DeepEqual(launch.Arguments, test.wantNative) {
						t.Fatalf("native argv = %q, want %q", launch.Arguments, test.wantNative)
					}
					if value := environmentValue(launch.Environment, "AGENT_SESSIONS_CODEX_PENDING_LAUNCH"); value != "" {
						t.Fatalf("pending token leaked into native environment: %q", value)
					}
					if value := environmentValue(launch.Environment, peerSessionIDEnv); value != "" {
						t.Fatalf("provisional session id reached native launch: %q", value)
					}
					result, confirmErr := launch.Confirm()
					if confirmErr != nil || result.ThreadID != pending.result.ThreadID {
						t.Fatalf("native confirmation = %+v, %v", result, confirmErr)
					}
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if request.SelectorKind != test.selectorKind || len(request.PendingToken) != 64 || !reflect.DeepEqual(sequence, []string{"pending", "native"}) {
				t.Fatalf("pending request = %+v, sequence = %v", request, sequence)
			}
			if !pending.awaited || !pending.closed {
				t.Fatalf("pending lifecycle awaited=%v closed=%v", pending.awaited, pending.closed)
			}
		})
	}
}

func TestCodexPeerUsageErrorsPrecedePendingDaemonContact(t *testing.T) {
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/true")
	contacts := 0
	err := RunCodexPeerWithDaemon(context.Background(), []string{"--peer-name", ""},
		func(context.Context, CodexDaemonPrepareRequest) (CodexPendingLaunch, error) {
			contacts++
			return nil, nil
		}, func(context.Context, CodexNativeLaunch) error {
			t.Fatal("native Codex started after a usage error")
			return nil
		})
	if err == nil || contacts != 0 {
		t.Fatalf("usage error = %v, daemon contacts = %d", err, contacts)
	}
}

func TestCodexResumeGroupsComeOnlyFromCurrentInvocation(t *testing.T) {
	root := t.TempDir()
	plan, err := parseCodexPeerArgs([]string{"resume", "00000000-0000-0000-0000-00000000c011"}, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.peerContext.groups) != 0 {
		t.Fatalf("resume inherited groups not present in argv: %v", plan.peerContext.groups)
	}
	plan, err = parseCodexPeerArgs([]string{"resume", "00000000-0000-0000-0000-00000000c011", "-g", "new"}, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.peerContext.groups, []string{"new"}) {
		t.Fatalf("resume groups = %v", plan.peerContext.groups)
	}
}

func TestCodexPeerInformationalAndPassthroughPreserveNativeArgv(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"-n", "ignored", "--version"},
		{"-C", root, "completion", "bash"},
		{"agents"},
		{"queue", "thread", "message"},
		{"app"},
		{"agents"},
		{"queue", "thread", "follow-up"},
		{"migrate-rollouts"},
	} {
		plan, err := parseCodexPeerArgs(args, root, "")
		if err != nil {
			t.Fatal(err)
		}
		if !plan.informationalPass && plan.mode != modePassthrough {
			t.Fatalf("not passthrough: %+v", plan)
		}
	}
}

func TestCodexPeerForwardsNativeOptionsAndRejectsUnattestedOperations(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"--model", "gpt-5", "prompt"}, want: []string{"--model", "gpt-5", "prompt"}},
		{args: []string{"--profile=testing", "--search", "resume", "thread", "--sandbox", "read-only"}, want: []string{"--profile=testing", "--search", "--sandbox", "read-only"}},
		{args: []string{"resume", "--all", "--model", "gpt-5", "thread", "--dangerously-bypass-hook-trust"}, want: []string{"--all", "--model", "gpt-5", "--dangerously-bypass-hook-trust"}},
		{args: []string{"-mresume", "resume", "thread", "-a", "never"}, want: []string{"-mresume", "-a", "never"}},
	} {
		plan, err := parseCodexPeerArgs(test.args, root, "")
		if err != nil {
			t.Fatalf("parse %q: %v", test.args, err)
		}
		if !reflect.DeepEqual(plan.interactiveArgs, test.want) {
			t.Fatalf("forwarded argv changed: got %q want %q", plan.interactiveArgs, test.want)
		}
	}
	for _, args := range [][]string{
		{"resume", "thread", "--remote", "tcp://host"},
		{"resume", "thread", "--remote-auth-token-env=TOKEN"},
	} {
		if _, err := parseCodexPeerArgs(args, root, ""); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
