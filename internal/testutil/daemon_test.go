package testutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/socketpath"
)

func TestDaemonFixtureUsesPrivateSocketSizedRoots(t *testing.T) {
	fixture := NewDaemonFixture(t)

	paths := fixture.Paths()
	for name, path := range map[string]string{
		"runtime root": paths.RuntimeRoot,
		"state root":   paths.StateRoot,
		"config root":  paths.ConfigRoot,
	} {
		if filepath.Dir(path) != paths.Root {
			t.Fatalf("%s %q is not immediately beneath fixture root %q", name, path, paths.Root)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode = %v, want a private directory", name, info.Mode())
		}
	}
	if paths.ControlEndpoint != filepath.Join(paths.RuntimeRoot, "daemon.sock") {
		t.Fatalf("control endpoint = %q, want runtime-root daemon.sock", paths.ControlEndpoint)
	}
	if !socketpath.Fits(paths.ControlEndpoint) {
		t.Fatalf("control endpoint %q exceeds platform socket-path limit %d", paths.ControlEndpoint, socketpath.Limit())
	}
}

func TestDaemonFixtureRunsAndStopsInProcess(t *testing.T) {
	fixture := NewDaemonFixture(t)
	started := make(chan int, 1)

	if err := fixture.Start(func(ctx context.Context, paths DaemonPaths) error {
		if paths != fixture.Paths() {
			return errors.New("runner received different daemon paths")
		}
		started <- os.Getpid()
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatalf("start fixture: %v", err)
	}

	select {
	case runnerPID := <-started:
		if runnerPID != os.Getpid() {
			t.Fatalf("runner pid = %d, test pid = %d; fixture started another process", runnerPID, os.Getpid())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-process daemon runner")
	}

	stopContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := fixture.Stop(stopContext); err != nil {
		t.Fatalf("stop fixture: %v", err)
	}
}

func TestDaemonFixtureRefusesSecondRunner(t *testing.T) {
	fixture := NewDaemonFixture(t)
	var calls atomic.Int32
	runner := func(ctx context.Context, _ DaemonPaths) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}

	if err := fixture.Start(runner); err != nil {
		t.Fatalf("start first runner: %v", err)
	}
	if err := fixture.Start(runner); !errors.Is(err, ErrDaemonFixtureStarted) {
		t.Fatalf("second start error = %v, want %v", err, ErrDaemonFixtureStarted)
	}

	stopContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := fixture.Stop(stopContext); err != nil {
		t.Fatalf("stop fixture: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
}

func TestDaemonFixtureWaitRequiresStart(t *testing.T) {
	fixture := NewDaemonFixture(t)
	waitContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fixture.Wait(waitContext); !errors.Is(err, ErrDaemonFixtureNotStarted) {
		t.Fatalf("wait error = %v, want %v", err, ErrDaemonFixtureNotStarted)
	}
}
