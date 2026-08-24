//go:build darwin

package qwenprofile

import (
	"fmt"
	"path/filepath"
)

// resolvePlatformPathAlias accepts only Darwin's fixed root-level aliases.
// Their directory entries live under / and cannot be retargeted by an
// unprivileged user. Requiring the exact native targets keeps arbitrary and
// user-controlled symlinks fail-closed.
func resolvePlatformPathAlias(path string) (string, bool, error) {
	var want string
	switch filepath.Clean(path) {
	case "/tmp":
		want = "/private/tmp"
	case "/var":
		want = "/private/var"
	default:
		return "", false, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, err
	}
	if resolved != want {
		return "", false, fmt.Errorf("resolved to %q, want fixed system target %q", resolved, want)
	}
	return resolved, true, nil
}
