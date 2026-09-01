//go:build darwin

package pathidentity

import (
	"fmt"
	"path/filepath"
)

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
