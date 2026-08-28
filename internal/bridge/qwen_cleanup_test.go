package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestQwenInteractiveCleanupAfterEveryExitClass(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(t *testing.T, fake fakeQwenProcess, commandPID int)
	}{
		{name: "normal exit", stop: func(_ *testing.T, fake fakeQwenProcess, _ int) {
			qwenFakeWriteMarker(fake.Paths.Stop, "stop\n")
		}},
		{name: "ctrl c", stop: func(t *testing.T, _ fakeQwenProcess, pid int) {
			if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "sigterm", stop: func(t *testing.T, _ fakeQwenProcess, pid int) {
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrapper sigkill", stop: func(t *testing.T, _ fakeQwenProcess, pid int) {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeQwenProcess(t)
			command := fake.command("--session-id", "11111111-2222-4333-8444-555555555555", "--json-file", fake.Paths.Events, "--input-file", fake.Paths.Input)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			qwenTestWaitForPath(t, fake.Paths.Ready, 2*time.Second)
			start := qwenTestWaitForProcessStart(t, command.Process.Pid, time.Second)
			owned, err := observeQwenOwnedArtifacts(fake.Paths.Root, []string{fake.Paths.Events, fake.Paths.Input, fake.Paths.Records})
			if err != nil {
				t.Fatal(err)
			}
			test.stop(t, fake, command.Process.Pid)
			_ = qwenTestWaitForCommand(t, done, 2*time.Second)
			if err := cleanupQwenOwnedArtifacts(qwenCleanupRequest{
				LifecyclePID: command.Process.Pid, LifecycleStart: start, Root: fake.Paths.Root, Artifacts: owned,
			}); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{fake.Paths.Events, fake.Paths.Input, fake.Paths.Records} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Qwen artifact survived %s: %s (%v)", test.name, path, err)
				}
			}
		})
	}
}

func TestQwenCleanupPreservesReplacedArtifactAndRecordsDebt(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "events.jsonl")
	input := filepath.Join(root, "input.jsonl")
	for _, path := range []string{events, input} {
		qwenTestCreatePrivateFile(t, path, nil)
	}
	owned, err := observeQwenOwnedArtifacts(root, []string{events, input})
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement-events.jsonl")
	qwenTestCreatePrivateFile(t, replacement, []byte("unrelated\n"))
	if err := os.Rename(replacement, events); err != nil {
		t.Fatal(err)
	}
	err = cleanupQwenOwnedArtifacts(qwenCleanupRequest{LifecyclePID: 999999, LifecycleStart: "absent-owner", Root: root, Artifacts: owned})
	var debt *qwenCleanupDebtError
	if !errors.As(err, &debt) || len(debt.Paths) != 1 || debt.Paths[0] != events {
		t.Fatalf("changed-artifact cleanup error = %#v", err)
	}
	if body, readErr := os.ReadFile(events); readErr != nil || string(body) != "unrelated\n" {
		t.Fatalf("replacement was removed or changed: %q, %v", body, readErr)
	}
	if _, statErr := os.Lstat(input); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unchanged owned input survived cleanup: %v", statErr)
	}
}

func TestQwenCleanupRejectsRecycledLifecycleIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "input.jsonl")
	qwenTestCreatePrivateFile(t, path, nil)
	owned, err := observeQwenOwnedArtifacts(root, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	previous := qwenCleanupProcessIdentity
	qwenCleanupProcessIdentity = func(_ int, _ string) qwenCleanupIdentityStatus {
		return qwenCleanupIdentityRecycled
	}
	t.Cleanup(func() { qwenCleanupProcessIdentity = previous })
	if err := cleanupQwenOwnedArtifacts(qwenCleanupRequest{
		LifecyclePID: os.Getpid(), LifecycleStart: "old-start", Root: root, Artifacts: owned,
	}); err == nil || !errors.Is(err, errQwenCleanupIdentityChanged) {
		t.Fatalf("recycled lifecycle cleanup = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("recycled process cleanup removed artifact: %v", err)
	}
}

func TestQwenCleanupCancellationDoesNotDependOnCallerContext(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "input.jsonl")
	qwenTestCreatePrivateFile(t, path, nil)
	owned, err := observeQwenOwnedArtifacts(root, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cleanupQwenOwnedArtifactsWithContext(ctx, qwenCleanupRequest{
		LifecyclePID: 999999, LifecycleStart: "stopped", Root: root, Artifacts: owned,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled wrapper left cleanup artifact: %v", err)
	}
}
