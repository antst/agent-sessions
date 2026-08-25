//go:build darwin

package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

const (
	darwinTestRootEnv    = "AGENT_SESSIONS_DARWIN_TEST_ROOT"
	darwinTestRootMarker = ".agent-sessions-test-root"
	darwinTestRootLock   = ".lock"
	darwinTestRootMagic  = "agent-sessions bridge test root v1\n"
)

type darwinTestRootLease struct {
	root string
	lock *os.File
}

// Darwin's default per-user TMPDIR leaves too little room for the Unix socket
// suffixes exercised by bridge tests. One top-level test process leases a
// compact private namespace; helper subprocesses inherit and reuse that lease
// instead of consuming another globally scarce one-character leaf.
func TestMain(m *testing.M) {
	if inherited := os.Getenv(darwinTestRootEnv); inherited != "" {
		if err := validateDarwinTestRoot(inherited, "/tmp"); err != nil {
			panic(fmt.Sprintf("invalid inherited Darwin bridge test root: %v", err))
		}
		if err := validateInheritedDarwinTestRootLease(inherited); err != nil {
			panic(fmt.Sprintf("inactive inherited Darwin bridge test root: %v", err))
		}
		setDarwinTestRuntimeRoot(inherited)
		os.Exit(m.Run())
	}

	lease, err := compactDarwinTestRoot()
	if err != nil {
		panic(err)
	}
	if err := os.Setenv(darwinTestRootEnv, lease.root); err != nil {
		lease.release()
		panic(err)
	}
	setDarwinTestRuntimeRoot(lease.root)
	code := m.Run()
	lease.release()
	os.Exit(code)
}

func validateInheritedDarwinTestRootLease(root string) error {
	lock, err := os.OpenFile(filepath.Join(root, darwinTestRootLock), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return errors.New("test root has no live parent lease")
}

func setDarwinTestRuntimeRoot(root string) {
	if err := os.Setenv("TMPDIR", root); err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_RUNTIME_DIR", root); err != nil {
		panic(err)
	}
}

func TestNativeSupervisorDefaultSocketIgnoresDarwinTMPDIR(t *testing.T) {
	root := os.Getenv(darwinTestRootEnv)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("CLAUDE_PEER_SUPERVISOR_SOCKET", "")
	t.Setenv("CLAUDE_PEER_APP_SERVER_SOCKET", "")
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "runtime-path-state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "runtime-path-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "runtime-path-codex"))

	var sockets []string
	for _, tempDir := range []string{
		"/var/folders/very/long/native/darwin/temp/directory/T",
		"",
		"/tmp",
	} {
		t.Setenv("TMPDIR", tempDir)
		paths := resolveNativePaths()
		if paths.runtimeDir != "/tmp" {
			t.Fatalf("TMPDIR %q selected runtime dir %q", tempDir, paths.runtimeDir)
		}
		sockets = append(sockets, paths.supervisorSock)
	}
	want := filepath.Join("/tmp", "ccp-"+strconv.Itoa(os.Getuid()), "supervisor-"+nativeProfileKey(filepath.Join(root, "runtime-path-codex"))+".sock")
	for index, socket := range sockets {
		if socket != want {
			t.Fatalf("Darwin default supervisor socket[%d] = %q, want %q", index, socket, want)
		}
	}
}

func compactDarwinTestRoot() (*darwinTestRootLease, error) {
	// Keep every one-byte leaf safe when a test path crosses a shell boundary.
	// The 64 choices preserve sun_path headroom while leaving enough leases for
	// hosts carrying marked roots from abruptly terminated older test binaries.
	const leaves = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_-"
	return claimDarwinTestRoot("/tmp", leaves)
}

func claimDarwinTestRoot(base, leaves string) (*darwinTestRootLease, error) {
	start := os.Getpid() % len(leaves)
	for offset := range len(leaves) {
		root := filepath.Join(base, string(leaves[(start+offset)%len(leaves)]))
		created, err := createOrValidateDarwinTestRoot(root, base)
		if err != nil {
			if errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, err
		}
		lock, err := os.OpenFile(filepath.Join(root, darwinTestRootLock), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			if created {
				_ = os.RemoveAll(root)
			}
			continue
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lock.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			return nil, err
		}
		if err := clearStaleDarwinTestRoot(root); err != nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, err
		}
		return &darwinTestRootLease{root: root, lock: lock}, nil
	}
	return nil, errors.New("no compact private Darwin test runtime directory is available")
}

func createOrValidateDarwinTestRoot(root, base string) (bool, error) {
	if err := os.Mkdir(root, 0700); err == nil {
		marker, markerErr := os.OpenFile(filepath.Join(root, darwinTestRootMarker), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if markerErr == nil {
			_, markerErr = marker.WriteString(darwinTestRootMagic)
			closeErr := marker.Close()
			if markerErr == nil {
				markerErr = closeErr
			}
		}
		if markerErr != nil {
			_ = os.RemoveAll(root)
			return false, markerErr
		}
		return true, nil
	} else if !os.IsExist(err) {
		return false, err
	}
	return false, validateDarwinTestRoot(root, base)
}

func validateDarwinTestRoot(root, base string) error {
	if filepath.Dir(root) != filepath.Clean(base) || len(filepath.Base(root)) != 1 {
		return os.ErrPermission
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Geteuid()) {
		return os.ErrPermission
	}
	marker, err := os.ReadFile(filepath.Join(root, darwinTestRootMarker))
	if err != nil || string(marker) != darwinTestRootMagic {
		return os.ErrPermission
	}
	return nil
}

func clearStaleDarwinTestRoot(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == darwinTestRootMarker || entry.Name() == darwinTestRootLock {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (lease *darwinTestRootLease) release() {
	if lease == nil || lease.lock == nil {
		return
	}
	// Remove the pathname while the lease is still locked. A new allocator may
	// then create a distinct inode without racing deletion of its live root.
	_ = os.RemoveAll(lease.root)
	_ = syscall.Flock(int(lease.lock.Fd()), syscall.LOCK_UN)
	_ = lease.lock.Close()
	lease.lock = nil
}

func TestDarwinTestRootLeaseReclaimsOnlyMarkedUnlockedRoots(t *testing.T) {
	base := t.TempDir()
	first, err := claimDarwinTestRoot(base, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInheritedDarwinTestRootLease(first.root); err != nil {
		first.release()
		t.Fatalf("live parent lease was not visible to a helper: %v", err)
	}
	if _, err := claimDarwinTestRoot(base, "x"); err == nil {
		first.release()
		t.Fatal("second process acquired a live Darwin test root lease")
	}
	stale := filepath.Join(first.root, "stale-helper-artifact")
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		first.release()
		t.Fatal(err)
	}
	// Simulate abrupt process exit: the kernel releases flock but the marked
	// directory and its helper residue remain.
	_ = syscall.Flock(int(first.lock.Fd()), syscall.LOCK_UN)
	_ = first.lock.Close()
	first.lock = nil
	if err := validateInheritedDarwinTestRootLease(first.root); err == nil {
		t.Fatal("stale inherited root was accepted without a live parent lease")
	}
	second, err := claimDarwinTestRoot(base, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale helper artifact survived lease recovery: %v", err)
	}
}
