package host

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateEndpointBridgeCancel(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	endpoint, err := ListenPrivate(socket, "session")
	must(t, err)
	path := endpoint.Path
	info, err := os.Stat(path)
	check(t, err == nil && path == filepath.Join(filepath.Dir(socket), "lanes", "session.sock") && info.Mode().Perm() == 0o600, "endpoint path/mode = %q/%v", path, err)
	received, release := make(chan string, 1), make(chan struct{})
	go func() {
		connection, _ := endpoint.Accept()
		defer connection.Close()
		body := make([]byte, len("frame"))
		_, _ = io.ReadFull(connection, body)
		received <- string(body)
		_, _ = connection.Write([]byte("blocked"))
		<-release
	}()
	input, inputWriter := io.Pipe()
	defer inputWriter.Close()
	output := blockedWriter{started: make(chan struct{}), release: release}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- BridgeStdio(ctx, path, input, output) }()
	_, _ = inputWriter.Write([]byte("frame"))
	check(t, <-received == "frame", "bridge input changed")
	<-output.started
	cancel()
	err = <-returned
	check(t, errors.Is(err, context.Canceled), "cancel error = %v", err)
	close(release)
	must(t, endpoint.Close())
	_, err = os.Stat(path)
	check(t, errors.Is(err, os.ErrNotExist), "endpoint remains: %v", err)
}

type blockedWriter struct{ started, release chan struct{} }

func (w blockedWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}
