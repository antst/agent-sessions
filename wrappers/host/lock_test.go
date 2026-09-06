package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionLockStaleFileAndContention(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	path := filepath.Join(filepath.Dir(socket), "locks", "example", "session")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("stale"), 0o600))
	lock, err := AcquireSessionLock(socket, "example", "session")
	must(t, err)
	must(t, lock.Close())
}

func TestSessionLockRenamePreservesClaim(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	lock, err := AcquireSessionLock(socket, "example", "provisional")
	must(t, err)
	must(t, lock.Rename("session"))
	_, err = AcquireSessionLock(socket, "example", "session")
	check(t, err != nil && err.Error() == "session busy", "renamed lock contention = %v", err)
	_, err = os.Stat(filepath.Join(filepath.Dir(socket), "locks", "example", "provisional"))
	check(t, os.IsNotExist(err), "provisional lock remains: %v", err)
	must(t, lock.Close())
}

func TestSessionLockOrderlyCloseAllowsResume(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	first, err := AcquireSessionLock(socket, "example", "first")
	must(t, err)
	must(t, first.Rename("session"))
	must(t, first.Close())
	resumed, err := AcquireSessionLock(socket, "example", "resumed")
	must(t, err)
	must(t, resumed.Rename("session"))
	must(t, resumed.Close())
}

func TestSessionLockRenameRecoversCrashStaleTarget(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	path := filepath.Join(filepath.Dir(socket), "locks", "example", "session")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("crash-stale"), 0o600))
	lock, err := AcquireSessionLock(socket, "example", "provisional")
	must(t, err)
	must(t, lock.Rename("session"))
	_, err = AcquireSessionLock(socket, "example", "session")
	check(t, err != nil && err.Error() == "session busy", "recovered target contention = %v", err)
	must(t, lock.Close())
}

func TestSessionLockRenameRetriesInodeMismatch(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	path := filepath.Join(filepath.Dir(socket), "locks", "example", "session")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("stale"), 0o600))
	lock, err := AcquireSessionLock(socket, "example", "provisional")
	must(t, err)
	comparisons := 0
	must(t, lock.rename("session", os.Link, func(opened, current os.FileInfo) bool {
		comparisons++
		return comparisons > 1 && os.SameFile(opened, current)
	}))
	check(t, comparisons == 2, "inode comparisons = %d", comparisons)
	must(t, lock.Close())
}

func TestSessionLockRenamePreservesRecreatedTarget(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	path := filepath.Join(filepath.Dir(socket), "locks", "example", "session")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("stale"), 0o600))
	lock, err := AcquireSessionLock(socket, "example", "provisional")
	must(t, err)
	links := 0
	err = lock.rename("session", func(old, target string) error {
		links++
		if links == 2 {
			must(t, os.WriteFile(target, []byte("recreated"), 0o600))
			return os.ErrExist
		}
		return os.Link(old, target)
	}, os.SameFile)
	check(t, err != nil && err.Error() == "session busy", "recreated target result = %v", err)
	check(t, links == 2, "link attempts = %d", links)
	recreated, err := AcquireSessionLock(socket, "example", "session")
	must(t, err)
	must(t, recreated.Close())
	must(t, lock.Close())
}

func TestSessionLockRenameDoesNotReplaceHolder(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	holder, err := AcquireSessionLock(socket, "example", "session")
	must(t, err)
	provisional, err := AcquireSessionLock(socket, "example", "provisional")
	must(t, err)
	err = provisional.Rename("session")
	check(t, err != nil && err.Error() == "session busy", "rename collision = %v", err)
	_, err = os.Stat(filepath.Join(filepath.Dir(socket), "locks", "example", "provisional"))
	check(t, os.IsNotExist(err), "provisional lock remains: %v", err)
	_, err = AcquireSessionLock(socket, "example", "session")
	check(t, err != nil && err.Error() == "session busy", "holder was replaced: %v", err)
	must(t, provisional.Close())
	must(t, holder.Close())
}
