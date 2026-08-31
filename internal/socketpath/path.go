// Package socketpath centralizes platform Unix-domain socket path budgets.
package socketpath

import (
	"fmt"
	"path/filepath"
)

// Validate checks that path is an absolute, clean Unix-domain socket address
// that fits the current platform's sockaddr_un.sun_path field, including the
// required trailing NUL byte.
func Validate(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("unix socket path is not absolute and clean: %s", path)
	}
	if len([]byte(path)) > Limit() {
		return fmt.Errorf("unix socket path is %d bytes; platform limit is %d: %s", len([]byte(path)), Limit(), path)
	}
	return nil
}

// Fits reports whether path satisfies Validate.
func Fits(path string) bool { return Validate(path) == nil }

// PreferRoot returns preferred when its longest socket-relative path fits the
// platform, otherwise fallback. Callers still validate the final concrete
// socket path before binding.
func PreferRoot(preferred, fallback, longestRelative string) string {
	if Fits(filepath.Join(preferred, longestRelative)) {
		return preferred
	}
	return fallback
}
