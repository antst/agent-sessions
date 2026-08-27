package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
)

func TestExecuteDaemonPreparedPeerDirectExecAndExactRollback(t *testing.T) {
	prepared := daemon.AttachmentPrepareResult{
		Attachment: daemon.AttachmentRecord{AttachmentID: "attachment-1"}, Capability: "capability-secret",
		Launch: daemon.NativeLaunchPlan{Executable: "/native/qwen", Arguments: []string{"--resume", "session-1"}, SessionID: "session-1", Cwd: "/workspace"},
	}
	var (
		executable  string
		arguments   []string
		environment []string
		detachedID  string
	)
	wantErr := errors.New("injected exec failure")
	err := executeDaemonPreparedPeer(context.Background(), "qwen", prepared, daemonPeerDependencies{
		exec: func(path string, args, env []string) error {
			executable, arguments, environment = path, append([]string(nil), args...), append([]string(nil), env...)
			return wantErr
		},
		detach: func(_ context.Context, attachmentID, reason string) error {
			detachedID = attachmentID
			if reason != "native_exec_failed" {
				t.Fatalf("detach reason = %q", reason)
			}
			return nil
		},
	})
	if !errors.Is(err, wantErr) || executable != "/native/qwen" || strings.Join(arguments, " ") != "--resume session-1" || detachedID != "attachment-1" {
		t.Fatalf("exec result: err=%v executable=%q arguments=%q detached=%q", err, executable, arguments, detachedID)
	}
	joined := strings.Join(environment, "\n")
	for _, want := range []string{
		internalAttachmentIDEnv + "=attachment-1", internalCapabilityEnv + "=capability-secret",
		internalProductEnv + "=qwen", internalSessionIDEnv + "=session-1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("prepared environment lacks %q", want)
		}
	}
	if strings.Contains(strings.Join(arguments, " "), "capability-secret") {
		t.Fatal("raw launch capability leaked into process arguments")
	}
}

//nolint:dupl // Each product test intentionally proves its own canonical launcher entrypoint.
func TestCodexPeerUsesDaemonPlanAndDirectNativeExec(t *testing.T) {
	cwd := t.TempDir()
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	var request daemon.AttachmentPrepareRequest
	var executed string
	err = runCodexPeer([]string{"-n", "codex-direct", "--yolo"}, daemonPeerDependencies{
		prepare: func(_ context.Context, candidate daemon.AttachmentPrepareRequest) (daemon.AttachmentPrepareResult, error) {
			request = candidate
			return daemon.AttachmentPrepareResult{
				Attachment: daemon.AttachmentRecord{AttachmentID: "codex-attachment"}, Capability: "codex-capability",
				Launch: daemon.NativeLaunchPlan{Executable: "/native/codex", Arguments: []string{"resume", "thread-1"}, Cwd: candidate.Cwd},
			}, nil
		},
		detach: func(context.Context, string, string) error { return nil },
		exec: func(path string, _ []string, _ []string) error {
			executed = path
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run Codex peer: %v", err)
	}
	if executed != "/native/codex" || request.Product != "codex" || request.Name != "codex-direct" || request.Cwd != cwd || request.PermissionMode != "bypassPermissions" {
		t.Fatalf("prepare=%#v executed=%q", request, executed)
	}
}

func TestGrokPeerUsesDaemonPlanWithoutGrokHost(t *testing.T) {
	cwd := t.TempDir()
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	var request daemon.AttachmentPrepareRequest
	var executed string
	err = runGrokPeerWithDaemon([]string{"-n", "grok-direct", "--always-approve"}, daemonPeerDependencies{
		prepare: func(_ context.Context, candidate daemon.AttachmentPrepareRequest) (daemon.AttachmentPrepareResult, error) {
			request = candidate
			return daemon.AttachmentPrepareResult{
				Attachment: daemon.AttachmentRecord{AttachmentID: "grok-attachment"}, Capability: "grok-capability",
				Launch: daemon.NativeLaunchPlan{Executable: "/native/grok", Arguments: []string{"--leader"}, Cwd: candidate.Cwd},
			}, nil
		},
		detach: func(context.Context, string, string) error { return nil },
		exec: func(path string, _ []string, _ []string) error {
			executed = path
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run Grok peer: %v", err)
	}
	if executed != "/native/grok" || request.Name != "grok-direct" || request.PermissionMode != "bypassPermissions" {
		t.Fatalf("prepare=%#v executed=%q", request, executed)
	}
}

//nolint:dupl // Each product test intentionally proves its own canonical launcher entrypoint.
func TestQwenPeerUsesDaemonPlanWithoutQwenHost(t *testing.T) {
	cwd := t.TempDir()
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	var request daemon.AttachmentPrepareRequest
	var executed string
	err = runQwenPeerWithDaemon([]string{"-n", "qwen-direct", "--approval-mode", "yolo"}, daemonPeerDependencies{
		prepare: func(_ context.Context, candidate daemon.AttachmentPrepareRequest) (daemon.AttachmentPrepareResult, error) {
			request = candidate
			return daemon.AttachmentPrepareResult{
				Attachment: daemon.AttachmentRecord{AttachmentID: "qwen-attachment"}, Capability: "qwen-capability",
				Launch: daemon.NativeLaunchPlan{Executable: "/native/qwen", Arguments: []string{"--session-id", "session-1"}, Cwd: candidate.Cwd},
			}, nil
		},
		detach: func(context.Context, string, string) error { return nil },
		exec: func(path string, _ []string, _ []string) error {
			executed = path
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run Qwen peer: %v", err)
	}
	if executed != "/native/qwen" || request.Name != "qwen-direct" || request.Product != "qwen" || request.PermissionMode != "yolo" {
		t.Fatalf("prepare=%#v executed=%q", request, executed)
	}
}

//nolint:dupl // Each product test intentionally proves its own canonical launcher entrypoint.
func TestClaudePeerUsesDaemonPlanWithoutLifecycleWrapper(t *testing.T) {
	cwd := t.TempDir()
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(cwd, "claude-config"))
	var request daemon.AttachmentPrepareRequest
	var executed string
	err = runClaudePeerWithDaemon([]string{"-n", "claude-direct", "--dangerously-skip-permissions"}, daemonPeerDependencies{
		prepare: func(_ context.Context, candidate daemon.AttachmentPrepareRequest) (daemon.AttachmentPrepareResult, error) {
			request = candidate
			return daemon.AttachmentPrepareResult{
				Attachment: daemon.AttachmentRecord{AttachmentID: "claude-attachment"}, Capability: "claude-capability",
				Launch: daemon.NativeLaunchPlan{Executable: "/native/claude", Arguments: []string{"--session-id", "session-1"}, Cwd: candidate.Cwd},
			}, nil
		},
		detach: func(context.Context, string, string) error { return nil },
		exec: func(path string, _ []string, _ []string) error {
			executed = path
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run Claude peer: %v", err)
	}
	if executed != "/native/claude" || request.Name != "claude-direct" || request.PermissionMode != "bypassPermissions" || request.Cwd != cwd {
		t.Fatalf("prepare=%#v executed=%q", request, executed)
	}
}
