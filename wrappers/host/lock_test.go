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
	if _, err = AcquireSessionLock(socket, "example", "session"); err == nil || err.Error() != "session busy" {
		t.Fatalf("contention error = %v", err)
	}
	must(t, lock.Close())
	lock, err = AcquireSessionLock(socket, "example", "session")
	must(t, err)
	if lock.File() == nil {
		t.Fatal("lock file unavailable for inheritance")
	}
	must(t, lock.Close())
}
