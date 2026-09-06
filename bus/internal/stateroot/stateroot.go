package stateroot

import (
	"os"
	"path/filepath"
)

// Resolve returns the conventional Sessionbus state root.
func Resolve() (string, error) {
	var root string
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		root = filepath.Join(xdg, "sessionbus")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state", "sessionbus")
	}
	return filepath.Abs(root)
}

// SessionSocket resolves the client-side socket discovery order.
func SessionSocket() (string, error) {
	if socket := os.Getenv("SESSIONBUS_SOCKET"); socket != "" {
		return filepath.Abs(socket)
	}
	root, err := Resolve()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "run", "presence.sock"), nil
}
