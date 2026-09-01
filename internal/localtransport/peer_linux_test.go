//go:build linux

package localtransport

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxPeerIdentityComesFromKernelSocketCredentials(t *testing.T) {
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

	accepted := make(chan PeerIdentity, 1)
	errs := make(chan error, 1)
	go func() {
		connection, peer, acceptErr := listener.Accept()
		if acceptErr != nil {
			errs <- acceptErr
			return
		}
		defer connection.Close()
		accepted <- peer
	}()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	select {
	case err := <-errs:
		t.Fatalf("Accept: %v", err)
	case peer := <-accepted:
		if peer.PID != os.Getpid() || peer.UID != os.Getuid() || !peer.Valid() {
			t.Fatalf("peer identity = %#v, want pid=%d uid=%d", peer, os.Getpid(), os.Getuid())
		}
	}
}
