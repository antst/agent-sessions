//go:build linux || darwin

package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/socketpath"
	"github.com/antst/agent-sessions/internal/testutil"
)

func TestProductionControlEndpointIsFixedAndTMPDIRIndependent(t *testing.T) {
	runtimeRoot := testutil.ShortSocketRoot(t, "asr-", filepath.Join("agent-sessions", "daemon.sock"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "first-choice-must-be-ignored"))

	first, err := productionControlEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "second-choice-must-also-be-ignored"))
	second, err := productionControlEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("TMPDIR redirected the production endpoint: first=%q second=%q", first, second)
	}

	var want string
	switch runtime.GOOS {
	case "linux":
		want = filepath.Join(runtimeRoot, "agent-sessions", "daemon.sock")
	case "darwin":
		want = fmt.Sprintf("/tmp/agent-sessions-%d/daemon.sock", os.Getuid())
	default:
		t.Fatalf("unexpected supported Unix platform %q", runtime.GOOS)
	}
	if first != want {
		t.Fatalf("productionControlEndpoint() = %q, want fixed endpoint %q", first, want)
	}
	if err := socketpath.Validate(first); err != nil {
		t.Fatalf("fixed endpoint exceeds the %s AF_UNIX budget: %v", runtime.GOOS, err)
	}
}

func TestLinuxProductionControlEndpointRequiresXDGRuntimeDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific XDG runtime contract")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	if endpoint, err := productionControlEndpoint(); err == nil {
		t.Fatalf("productionControlEndpoint() = %q without XDG_RUNTIME_DIR; TMPDIR must not be a fallback", endpoint)
	}
}

func TestAcquireControlEndpointRejectsDuplicateDaemonWithoutTouchingLiveSocket(t *testing.T) {
	endpoint := filepath.Join(testutil.ShortSocketRoot(t, "asc-", "daemon.sock"), "daemon.sock")
	first, err := acquireControlEndpoint(controlEndpointOptions{endpoint: endpoint})
	if err != nil {
		t.Fatalf("acquire first endpoint: %v", err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Errorf("close first endpoint: %v", err)
		}
	})

	lockPath := mustControlEndpointLockPath(t, endpoint)
	lockInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("inspect adjacent control lock: %v", err)
	}
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("control lock mode = %s, want a regular file", lockInfo.Mode())
	}
	if got := lockInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("control lock permissions = %04o, want 0600", got)
	}
	assertExclusiveControlLockHeld(t, lockPath)

	before := mustSocketInfo(t, endpoint)
	second, err := acquireControlEndpoint(controlEndpointOptions{endpoint: endpoint})
	if err == nil {
		_ = second.Close()
		t.Fatal("second daemon unexpectedly acquired the live endpoint")
	}
	after := mustSocketInfo(t, endpoint)
	if !os.SameFile(before, after) {
		t.Fatal("duplicate acquisition replaced the live daemon socket")
	}
	connection, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		t.Fatalf("first daemon socket stopped accepting connections after duplicate attempt: %v", err)
	}
	_ = connection.Close()
}

func TestAcquireControlEndpointLocksBeforeUnlinkingOrBinding(t *testing.T) {
	endpoint := filepath.Join(testutil.ShortSocketRoot(t, "asc-", "daemon.sock"), "daemon.sock")
	leaveOrphanedUnixSocket(t, endpoint)
	before := mustSocketInfo(t, endpoint)

	lockPath := mustControlEndpointLockPath(t, endpoint)
	blocker, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create adjacent control lock: %v", err)
	}
	blockerOpen := true
	t.Cleanup(func() {
		if blockerOpen {
			_ = syscall.Flock(int(blocker.Fd()), syscall.LOCK_UN)
			_ = blocker.Close()
		}
	})
	if err := syscall.Flock(int(blocker.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold adjacent control lock: %v", err)
	}

	prior := procinfo.Process{
		PID: 424242,
		Info: procinfo.Info{
			Status:      procinfo.Known,
			Start:       "recorded-start",
			StrongStart: "recorded-strong-start",
		},
	}
	options := controlEndpointOptions{
		endpoint:      endpoint,
		priorIdentity: &prior,
		processInfo: func(int) (procinfo.Info, error) {
			return procinfo.Info{Status: procinfo.Absent}, nil
		},
	}
	if owned, err := acquireControlEndpoint(options); err == nil {
		_ = owned.Close()
		t.Fatal("endpoint acquisition ignored the already-held adjacent lock")
	}
	after := mustSocketInfo(t, endpoint)
	if !os.SameFile(before, after) {
		t.Fatal("endpoint was unlinked or rebound before acquiring the adjacent lock")
	}

	if err := syscall.Flock(int(blocker.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("release adjacent control lock: %v", err)
	}
	if err := blocker.Close(); err != nil {
		t.Fatalf("close adjacent control lock: %v", err)
	}
	blockerOpen = false
	owned, err := acquireControlEndpoint(options)
	if err != nil {
		t.Fatalf("acquire corroborated stale endpoint after lock release: %v", err)
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("close replacement endpoint: %v", err)
	}
}

func TestAcquireControlEndpointRemovesStaleSocketOnlyWithExactAbsentIdentity(t *testing.T) {
	prior := procinfo.Process{
		PID: 424242,
		Info: procinfo.Info{
			Status:      procinfo.Known,
			Start:       "recorded-start",
			StrongStart: "recorded-strong-start",
		},
	}
	tests := []struct {
		name          string
		priorIdentity *procinfo.Process
		processInfo   func(int) (procinfo.Info, error)
		wantAcquire   bool
	}{
		{
			name: "missing durable identity",
		},
		{
			name:          "identity cannot be inspected",
			priorIdentity: &prior,
			processInfo: func(int) (procinfo.Info, error) {
				return procinfo.Info{Status: procinfo.Unknown}, nil
			},
		},
		{
			name:          "recorded daemon is still live",
			priorIdentity: &prior,
			processInfo: func(pid int) (procinfo.Info, error) {
				if pid != prior.PID {
					return procinfo.Info{Status: procinfo.Unknown}, fmt.Errorf("process probe PID = %d, want %d", pid, prior.PID)
				}
				return prior.Info, nil
			},
		},
		{
			name:          "PID was reused by a different process identity",
			priorIdentity: &prior,
			processInfo: func(pid int) (procinfo.Info, error) {
				if pid != prior.PID {
					return procinfo.Info{Status: procinfo.Unknown}, fmt.Errorf("process probe PID = %d, want %d", pid, prior.PID)
				}
				return procinfo.Info{
					Status:      procinfo.Known,
					Start:       "replacement-start",
					StrongStart: "replacement-strong-start",
				}, nil
			},
			wantAcquire: true,
		},
		{
			name:          "exact prior daemon is proven absent",
			priorIdentity: &prior,
			processInfo: func(pid int) (procinfo.Info, error) {
				if pid != prior.PID {
					return procinfo.Info{Status: procinfo.Unknown}, fmt.Errorf("process probe PID = %d, want %d", pid, prior.PID)
				}
				return procinfo.Info{Status: procinfo.Absent}, nil
			},
			wantAcquire: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint := filepath.Join(testutil.ShortSocketRoot(t, "asc-", "daemon.sock"), "daemon.sock")
			leaveOrphanedUnixSocket(t, endpoint)
			before := mustSocketInfo(t, endpoint)

			owned, err := acquireControlEndpoint(controlEndpointOptions{
				endpoint:      endpoint,
				priorIdentity: test.priorIdentity,
				processInfo:   test.processInfo,
			})
			if test.wantAcquire {
				if err != nil {
					t.Fatalf("replace corroborated stale endpoint: %v", err)
				}
				t.Cleanup(func() {
					if closeErr := owned.Close(); closeErr != nil {
						t.Errorf("close replacement endpoint: %v", closeErr)
					}
				})
				if _, statErr := os.Lstat(endpoint); statErr != nil {
					t.Fatalf("replacement socket is absent: %v", statErr)
				}
				connection, dialErr := net.DialTimeout("unix", endpoint, time.Second)
				if dialErr != nil {
					t.Fatalf("replacement socket is not accepting connections: %v", dialErr)
				}
				_ = connection.Close()
				return
			}

			if err == nil {
				_ = owned.Close()
				t.Fatal("uncorroborated socket identity was replaced")
			}
			after := mustSocketInfo(t, endpoint)
			if !os.SameFile(before, after) {
				t.Fatal("failed stale-identity check touched the existing socket")
			}
		})
	}
}

func TestAcquireControlEndpointEnforcesAFUnixBudgetBeforeCreatingArtifacts(t *testing.T) {
	root := testutil.ShortSocketRoot(t, "asc-", "maximum.sock")
	leafBytes := socketpath.Limit() - len([]byte(root)) - 1
	if leafBytes < 1 {
		t.Fatalf("test root %q leaves no AF_UNIX path budget", root)
	}
	maximum := filepath.Join(root, strings.Repeat("s", leafBytes))
	if got := len([]byte(maximum)); got != socketpath.Limit() {
		t.Fatalf("maximum test endpoint is %d bytes, want %d", got, socketpath.Limit())
	}
	owned, err := acquireControlEndpoint(controlEndpointOptions{endpoint: maximum})
	if err != nil {
		t.Fatalf("acquire endpoint at exact AF_UNIX limit: %v", err)
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("close maximum-length endpoint: %v", err)
	}

	overlong := maximum + "x"
	if owned, err := acquireControlEndpoint(controlEndpointOptions{endpoint: overlong}); err == nil {
		_ = owned.Close()
		t.Fatal("endpoint beyond the AF_UNIX path budget was accepted")
	}
	if _, err := os.Lstat(overlong); !os.IsNotExist(err) {
		t.Fatalf("overlong endpoint created a filesystem artifact: %v", err)
	}
}

func leaveOrphanedUnixSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("create orphaned socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("orphan socket listener close: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

func mustSocketInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("inspect socket %q: %v", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%q mode = %s, want Unix socket", path, info.Mode())
	}
	return info
}

func mustControlEndpointLockPath(t *testing.T, endpoint string) string {
	t.Helper()
	lockPath := controlEndpointLockPath(endpoint)
	if lockPath == "" || !filepath.IsAbs(lockPath) || filepath.Clean(lockPath) != lockPath {
		t.Fatalf("controlEndpointLockPath(%q) = %q, want a clean absolute path", endpoint, lockPath)
	}
	if lockPath == endpoint || filepath.Dir(lockPath) != filepath.Dir(endpoint) {
		t.Fatalf("control lock %q is not distinct and adjacent to endpoint %q", lockPath, endpoint)
	}
	if again := controlEndpointLockPath(endpoint); again != lockPath {
		t.Fatalf("control lock derivation changed: first=%q second=%q", lockPath, again)
	}
	return lockPath
}

func assertExclusiveControlLockHeld(t *testing.T, lockPath string) {
	t.Helper()
	challenger, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open adjacent control lock challenger: %v", err)
	}
	defer func() { _ = challenger.Close() }()
	err = syscall.Flock(int(challenger.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(challenger.Fd()), syscall.LOCK_UN)
		t.Fatal("acquired a second exclusive lock while the control endpoint was owned")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("second exclusive lock error = %v, want would-block", err)
	}
}
