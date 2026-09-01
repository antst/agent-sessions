//go:build darwin

package localtransport

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinPeerIdentityCombinesPeerPIDAndEffectiveUID(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "component.sock")
	listener, err := Listen(path, DefaultLimits())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	result := make(chan PeerIdentity, 1)
	errs := make(chan error, 1)
	go func() {
		connection, peer, acceptErr := listener.Accept()
		if acceptErr != nil {
			errs <- acceptErr
			return
		}
		defer connection.Close()
		result <- peer
	}()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	select {
	case err := <-errs:
		t.Fatalf("Accept: %v", err)
	case peer := <-result:
		if peer.PID != os.Getpid() || peer.UID != os.Geteuid() || !peer.Valid() {
			t.Fatalf("peer identity = %#v, want pid=%d euid=%d", peer, os.Getpid(), os.Geteuid())
		}
	}
}

func TestDarwinSocketpairPeerCredentialsAreExact(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_LOCAL, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	leftFile := os.NewFile(uintptr(fds[0]), "component-peer-left")
	rightFile := os.NewFile(uintptr(fds[1]), "component-peer-right")
	defer leftFile.Close()
	defer rightFile.Close()
	leftConnection, err := net.FileConn(leftFile)
	if err != nil {
		t.Fatalf("FileConn(left): %v", err)
	}
	defer leftConnection.Close()
	rightConnection, err := net.FileConn(rightFile)
	if err != nil {
		t.Fatalf("FileConn(right): %v", err)
	}
	defer rightConnection.Close()
	leftUnix, ok := leftConnection.(*net.UnixConn)
	if !ok {
		t.Fatalf("left socketpair connection type = %T", leftConnection)
	}
	peer, err := CapturePeerIdentity(leftUnix)
	if err != nil {
		t.Fatalf("CapturePeerIdentity: %v", err)
	}
	if peer.PID != os.Getpid() || peer.UID != os.Geteuid() {
		t.Fatalf("socketpair peer = %#v, want pid=%d uid=%d", peer, os.Getpid(), os.Geteuid())
	}
}
