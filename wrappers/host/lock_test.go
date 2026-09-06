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
