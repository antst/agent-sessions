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
