package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateEndpointAndStdioBridge(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	endpoint, err := ListenPrivate(socket, "session")
	must(t, err)
	path := endpoint.Path
	done := make(chan error, 1)
	go func() {
		connection, acceptErr := endpoint.Accept()
		if acceptErr == nil {
			_, acceptErr = io.Copy(connection, connection)
			_ = connection.Close()
		}
		done <- acceptErr
	}()
	var output bytes.Buffer
	must(t, BridgeStdio(context.Background(), endpoint.Path, io.NopCloser(strings.NewReader("frame")), &output))
	must(t, <-done)
	if output.String() != "frame" {
		t.Fatalf("output = %q", output.String())
	}
	must(t, endpoint.Close())
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint remains: %v", err)
	}
}
