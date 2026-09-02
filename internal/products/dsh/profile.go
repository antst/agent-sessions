package dsh

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/productruntime"
)

// validateProfileIdentity is deliberately lexical. Product constructors may
// use it without taking a snapshot of the installed product or profile state.
func validateProfileIdentity(profile string) error {
	if profile == "" || profile == "." || profile == ".." || len(profile) > maxProfileBytes || profile != strings.TrimSpace(profile) || strings.ContainsAny(profile, "/\\") {
		return fmt.Errorf("%w: DSH profile identity is invalid", productruntime.ErrNativeRejected)
	}
	for _, character := range profile {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%w: DSH profile identity contains control characters", productruntime.ErrNativeRejected)
		}
	}
	return nil
}

// validateManagedDSHHomeShape checks only the authored path contract. Whether
// the directory exists and where symlinks resolve are live operation facts.
func validateManagedDSHHomeShape(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || temporaryPathLexical(path) {
		return fmt.Errorf("%w: DSH home must be a canonical Agent Sessions HOME/XDG-state path", productruntime.ErrUnsupportedPolicy)
	}
	return nil
}

func temporaryPathLexical(path string) bool {
	return within(filepath.Clean(path), "/tmp") || within(filepath.Clean(path), "/private/tmp")
}

func within(path, root string) bool {
	root = filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
