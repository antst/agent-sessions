// Package fileutil provides durable file operations shared by Agent Sessions
// processes without imposing caller-specific directory policy.
package fileutil

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WriteJSONAtomic replaces path with an owner-only, indented JSON document.
// The caller owns creation and permission policy for the parent directory.
func WriteJSONAtomic(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".agent-sessions-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(body, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
