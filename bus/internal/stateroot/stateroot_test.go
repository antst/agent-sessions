package stateroot

import (
	"path/filepath"
	"testing"
)

func TestSessionSocketUsesTheDocumentedDiscoveryOrder(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg"))
	explicit := filepath.Join(t.TempDir(), "session.sock")
	t.Setenv("AGENTBUS_SOCKET", explicit)
	if got, err := SessionSocket(); err != nil || got != explicit {
		t.Fatalf("explicit socket = %q, %v", got, err)
	}

	t.Setenv("AGENTBUS_SOCKET", "")
	xdg := filepath.Join(t.TempDir(), "selected-xdg")
	t.Setenv("XDG_STATE_HOME", xdg)
	if got, err := SessionSocket(); err != nil || got != filepath.Join(xdg, "agentbus", "run", "presence.sock") {
		t.Fatalf("XDG socket = %q, %v", got, err)
	}
}
