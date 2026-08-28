package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func TestDaemonLaneNormalizersReuseProductOptionContracts(t *testing.T) {
	schema := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	qwenHome, qwenRuntime := filepath.Join(t.TempDir(), "qwen"), filepath.Join(t.TempDir(), "runtime")
	for _, path := range []string{qwenHome, qwenRuntime} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	qwenHome, err = filepath.EvalSymlinks(qwenHome)
	if err != nil {
		t.Fatal(err)
	}
	qwenRuntime, err = filepath.EvalSymlinks(qwenRuntime)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_RUNTIME_DIR", qwenRuntime)

	for _, test := range []struct {
		name      string
		normalize func(context.Context, daemonpkg.LaneCommandNormalizationRequest) (daemonpkg.LaneCommandNormalization, error)
		request   daemonpkg.LaneCommandNormalizationRequest
		assert    func(*testing.T, daemonpkg.LaneCommandNormalization)
	}{
		{
			name: "codex", normalize: (&codexDaemonAdapter{}).NormalizeLaneCommand,
			request: daemonpkg.LaneCommandNormalizationRequest{Product: "codex", Command: "start", Name: "codex-worker", Cwd: t.TempDir(),
				Arguments: []string{"--name", "codex-worker", "--model", "gpt-native", "--effort", "high", "--sandbox", "workspace-write", "--approval-policy", "never", "--config", "features.test=true", "--web", "--schema", schema}},
			assert: func(t *testing.T, result daemonpkg.LaneCommandNormalization) {
				if stringValue(result.NativeOptions["model"]) != "gpt-native" || stringValue(result.NativeOptions["effort"]) != "high" ||
					stringValue(result.NativeOptions["sandbox"]) != "workspace-write" || !boolValue(result.NativeOptions["web"]) || result.NativeOptions["output_schema"] == nil {
					t.Fatalf("Codex normalization = %#v", result)
				}
			},
		},
		{
			name: "claude", normalize: (&claudeDaemonAdapter{}).NormalizeLaneCommand,
			request: daemonpkg.LaneCommandNormalizationRequest{Product: "claude", Command: "start", Name: "claude-worker", Cwd: t.TempDir(), PermissionMode: "dontAsk",
				Arguments: []string{"--name", "claude-worker", "--model", "opus", "--effort", "high", "--permission-mode", "plan", "--max-budget-usd", "2.5", "--tools", "Bash", "--allowed-tools", "Read", "--disallowed-tools", "Write", "--schema", schema}},
			assert: func(t *testing.T, result daemonpkg.LaneCommandNormalization) {
				if result.PermissionMode != "plan" || stringValue(result.NativeOptions["model"]) != "opus" ||
					stringValue(result.NativeOptions["max_budget_usd"]) != "2.5" || result.NativeOptions["output_schema"] == nil {
					t.Fatalf("Claude normalization = %#v", result)
				}
			},
		},
		{
			name: "grok", normalize: (&grokDaemonAdapter{}).NormalizeLaneCommand,
			request: daemonpkg.LaneCommandNormalizationRequest{Product: "grok", Command: "start", Name: "grok-worker", Cwd: t.TempDir(),
				Arguments: []string{"--name", "grok-worker", "--model", "grok-native", "--reasoning-effort", "high", "--permission-mode", "bypassPermissions"}},
			assert: func(t *testing.T, result daemonpkg.LaneCommandNormalization) {
				if result.PermissionMode != "bypassPermissions" || stringValue(result.NativeOptions["model"]) != "grok-native" ||
					stringValue(result.NativeOptions["reasoning_effort"]) != "high" {
					t.Fatalf("Grok normalization = %#v", result)
				}
			},
		},
		{
			name: "qwen", normalize: (&qwenDaemonAdapter{}).NormalizeLaneCommand,
			request: daemonpkg.LaneCommandNormalizationRequest{Product: "qwen", Command: "start", Name: "qwen-worker", Cwd: t.TempDir(), PermissionMode: "default",
				Arguments: []string{"--name", "qwen-worker", "--qwen-home", qwenHome, "--yolo"}},
			assert: func(t *testing.T, result daemonpkg.LaneCommandNormalization) {
				if result.PermissionMode != "yolo" || !boolValue(result.NativeOptions["qwen_home_set"]) ||
					stringValue(result.NativeOptions["qwen_home"]) != qwenHome || stringValue(result.NativeOptions["qwen_runtime_dir"]) != qwenRuntime {
					t.Fatalf("Qwen normalization = %#v", result)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.normalize(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			test.assert(t, result)
		})
	}
}

func TestCodexDaemonLaneNormalizerMaterializesAndRollsBackWorktree(t *testing.T) {
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(t.TempDir(), "agent-sessions-data"))
	repository := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-m", "initial"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	result, err := (&codexDaemonAdapter{}).NormalizeLaneCommand(context.Background(), daemonpkg.LaneCommandNormalizationRequest{
		Product: "codex", Command: "start", Name: "worktree-worker", Cwd: repository,
		Arguments: []string{"--name", "worktree-worker", "--worktree"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cwd == repository || stringValue(result.NativeOptions["worktree_path"]) != result.Cwd {
		t.Fatalf("worktree normalization = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(result.Cwd, "tracked.txt")); err != nil {
		t.Fatalf("materialized worktree: %v", err)
	}
	if result.Rollback == nil {
		t.Fatal("worktree normalization omitted pre-accept rollback")
	}
	if err := result.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(result.Cwd); !os.IsNotExist(err) {
		t.Fatalf("rolled back worktree still exists: %v", err)
	}
}

func TestQwenDaemonLaneNormalizerRejectsChangedResumeProfile(t *testing.T) {
	qwenHome := filepath.Join(t.TempDir(), "qwen")
	if err := os.Mkdir(qwenHome, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (&qwenDaemonAdapter{}).NormalizeLaneCommand(context.Background(), daemonpkg.LaneCommandNormalizationRequest{
		Product: "qwen", Command: "resume", LaneSessionID: "qwen-lane", Name: "worker", Cwd: t.TempDir(), PermissionMode: "default",
		Arguments: []string{"qwen-lane", "--qwen-home", qwenHome},
		NativeActor: map[string]any{
			"profile": "different-profile", "qwen_home_set": true, "qwen_home": filepath.Join(t.TempDir(), "other-qwen"),
		},
	})
	if err == nil {
		t.Fatal("Qwen resume accepted a changed native profile")
	}
}
