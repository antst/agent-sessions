package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestStartSweepsStaleSocketsAndLeavesLiveOnes(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "agentbus.sock")
	lanes := filepath.Join(directory, "lanes")
	if err := os.Mkdir(lanes, 0o700); err != nil {
		t.Fatal(err)
	}
	leaveStaleSocket(t, socket)
	staleLane := filepath.Join(lanes, "stale.sock")
	leaveStaleSocket(t, staleLane)
	liveLane, err := net.Listen("unix", filepath.Join(lanes, "live.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer liveLane.Close()
	daemon, err := Start(Config{SocketPath: socket, TablePath: filepath.Join(directory, "sessions")})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if _, err = os.Stat(staleLane); !os.IsNotExist(err) {
		t.Fatalf("stale lane remains: %v", err)
	}
	if fd, dialErr := net.Dial("unix", liveLane.Addr().String()); dialErr != nil {
		t.Fatalf("live lane removed: %v", dialErr)
	} else {
		_ = fd.Close()
	}
}

func leaveStaleSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
}
