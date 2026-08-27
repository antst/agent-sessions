package testutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var (
	// ErrDaemonFixtureStarted is returned when a test tries to attach a second
	// runner to one fixture.
	ErrDaemonFixtureStarted = errors.New("in-process daemon fixture already started")
	// ErrDaemonFixtureNotStarted is returned when a test waits before starting
	// the fixture runner.
	ErrDaemonFixtureNotStarted = errors.New("in-process daemon fixture not started")
)

// DaemonPaths are test-owned path overrides for one in-process daemon.
// Production endpoint discovery must not use these paths.
type DaemonPaths struct {
	Root            string
	RuntimeRoot     string
	StateRoot       string
	ConfigRoot      string
	ControlEndpoint string
}

// DaemonRunner is a daemon entry point invoked as a goroutine in the test
// process. The fixture deliberately provides no executable or service-manager
// launch API, so package tests cannot accidentally start a second user daemon.
type DaemonRunner func(context.Context, DaemonPaths) error

// DaemonFixture owns isolated paths and the lifecycle of one in-process daemon
// runner. A fixture may be started exactly once.
type DaemonFixture struct {
	paths DaemonPaths

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
	exitErr error
}

// NewDaemonFixture creates private state, configuration, and runtime roots. Its
// control endpoint is short enough for the current platform's Unix-socket
// limit and cannot collide with the production per-user endpoint.
func NewDaemonFixture(t testing.TB) *DaemonFixture {
	t.Helper()

	root := ShortSocketRoot(t, "asd-", filepath.Join("runtime", "daemon.sock"))
	paths := DaemonPaths{
		Root:        root,
		RuntimeRoot: filepath.Join(root, "runtime"),
		StateRoot:   filepath.Join(root, "state"),
		ConfigRoot:  filepath.Join(root, "config"),
	}
	paths.ControlEndpoint = filepath.Join(paths.RuntimeRoot, "daemon.sock")
	for _, directory := range []string{paths.RuntimeRoot, paths.StateRoot, paths.ConfigRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create in-process daemon fixture directory %q: %v", directory, err)
		}
	}

	fixture := &DaemonFixture{paths: paths}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fixture.Stop(ctx); err != nil {
			t.Errorf("stop in-process daemon fixture: %v", err)
		}
	})
	return fixture
}

// Paths returns the immutable, test-owned paths supplied to the runner.
func (f *DaemonFixture) Paths() DaemonPaths {
	return f.paths
}

// Start invokes runner in a goroutine in the current process. Start refuses a
// second runner even after the first runner exits.
func (f *DaemonFixture) Start(runner DaemonRunner) error {
	if runner == nil {
		return errors.New("in-process daemon runner is nil")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started {
		return ErrDaemonFixtureStarted
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.started = true
	f.cancel = cancel
	f.done = make(chan struct{})
	go func() {
		err := runner(ctx, f.paths)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		f.mu.Lock()
		f.exitErr = err
		close(f.done)
		f.mu.Unlock()
	}()
	return nil
}

// Wait waits for the in-process runner to exit or for ctx to expire.
func (f *DaemonFixture) Wait(ctx context.Context) error {
	f.mu.Lock()
	if !f.started {
		f.mu.Unlock()
		return ErrDaemonFixtureNotStarted
	}
	done := f.done
	f.mu.Unlock()

	select {
	case <-done:
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.exitErr
	case <-ctx.Done():
		return fmt.Errorf("wait for in-process daemon fixture: %w", ctx.Err())
	}
}

// Stop requests cancellation and waits for the in-process runner. Stopping an
// unstarted or already-stopped fixture is safe.
func (f *DaemonFixture) Stop(ctx context.Context) error {
	f.mu.Lock()
	if !f.started {
		f.mu.Unlock()
		return nil
	}
	cancel := f.cancel
	f.mu.Unlock()

	cancel()
	return f.Wait(ctx)
}
