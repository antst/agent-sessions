package stateroot

import (
	"path/filepath"
	"testing"
)

func TestPresenceSocketUsesTheDocumentedDiscoveryOrder(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg"))
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", filepath.Join(t.TempDir(), "state"))
	explicit := filepath.Join(t.TempDir(), "presence.sock")
	t.Setenv("AGENT_SESSIONS_PRESENCE_SOCKET", explicit)
	if got, err := PresenceSocket(); err != nil || got != explicit {
		t.Fatalf("explicit socket = %q, %v", got, err)
	}

	t.Setenv("AGENT_SESSIONS_PRESENCE_SOCKET", "")
	state := filepath.Join(t.TempDir(), "selected-state")
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", state)
	if got, err := PresenceSocket(); err != nil || got != filepath.Join(state, "run", "presence.sock") {
		t.Fatalf("state-root socket = %q, %v", got, err)
	}

	t.Setenv("AGENT_SESSIONS_STATE_ROOT", "")
	xdg := filepath.Join(t.TempDir(), "selected-xdg")
	t.Setenv("XDG_STATE_HOME", xdg)
	if got, err := PresenceSocket(); err != nil || got != filepath.Join(xdg, "agent-sessions", "run", "presence.sock") {
		t.Fatalf("XDG socket = %q, %v", got, err)
	}
}
