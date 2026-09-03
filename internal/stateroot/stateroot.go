package stateroot

import (
	"os"
	"path/filepath"
)

// Resolve returns the one conventional Agent Sessions state root.
func Resolve() (string, error) {
	root := os.Getenv("AGENT_SESSIONS_STATE_ROOT")
	if root == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			root = filepath.Join(xdg, "agent-sessions")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			root = filepath.Join(home, ".local", "state", "agent-sessions")
		}
	}
	return filepath.Abs(root)
}

// PresenceSocket resolves the one client-side socket-discovery order.
func PresenceSocket() (string, error) {
	if socket := os.Getenv("AGENT_SESSIONS_PRESENCE_SOCKET"); socket != "" {
		return filepath.Abs(socket)
	}
	root, err := Resolve()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "run", "presence.sock"), nil
}
