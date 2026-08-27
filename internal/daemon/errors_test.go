package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDialControlEndpointReportsUnavailableWithoutStartingAnything(t *testing.T) {
	endpoint := filepath.Join(t.TempDir(), "missing.sock")
	connection, err := DialControlEndpoint(context.Background(), endpoint)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("missing daemon returned a connection")
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("error = %v, want ErrDaemonUnavailable", err)
	}
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error type = %T, want *UnavailableError", err)
	}
	if unavailable.Endpoint != endpoint || unavailable.ExitCode() != 3 {
		t.Fatalf("unavailable error = %#v", unavailable)
	}
	if unavailable.NextAction == "" || !strings.Contains(err.Error(), unavailable.NextAction) {
		t.Fatalf("error lacks inspection action: %v", err)
	}
	if _, statErr := filepath.Glob(filepath.Join(filepath.Dir(endpoint), "*")); statErr != nil {
		t.Fatalf("inspect endpoint parent: %v", statErr)
	}
	entries, statErr := filepath.Glob(filepath.Join(filepath.Dir(endpoint), "*"))
	if statErr != nil {
		t.Fatalf("inspect endpoint parent: %v", statErr)
	}
	if len(entries) != 0 {
		t.Fatalf("unavailable dial created artifacts: %v", entries)
	}
}
