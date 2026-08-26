// Package pathidentity centralizes host-path canonicalization contracts shared
// by product adapters. Existing working directories follow their real target;
// durable future paths reject mutable symlink components.
package pathidentity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ExistingDirectory returns the absolute real path of an existing directory.
// Native products may report this spelling even when the operator entered a
// symlinked alias such as macOS /tmp.
func ExistingDirectory(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect path: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(canonical), nil
}

// FuturePath returns a durable absolute identity for a path whose leaf may not
// exist yet. Mutable symlink components fail closed. Platform-owned fixed
// aliases may resolve to their native target through resolvePlatformPathAlias.
func FuturePath(value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", errors.New("path must be a non-empty absolute path")
	}
	clean := filepath.Clean(value)
	volume := filepath.VolumeName(clean)
	relative := clean[len(volume):]
	current := volume + string(filepath.Separator)
	components := splitPath(relative)
	for index, component := range components {
		next := filepath.Join(current, component)
		info, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return filepath.Join(append([]string{current}, components[index:]...)...), nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect path component %q: %w", next, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, allowed, resolveErr := resolvePlatformPathAlias(next)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve platform path alias %q: %w", next, resolveErr)
			}
			if !allowed {
				return "", fmt.Errorf("path contains mutable symlink component %q", next)
			}
			current = resolved
			continue
		}
		current = next
	}
	return current, nil
}

func splitPath(path string) []string {
	result := make([]string, 0)
	for {
		dir, base := filepath.Split(path)
		if base != "" {
			result = append(result, base)
		}
		path = filepath.Clean(dir)
		if path == "." || path == string(filepath.Separator) || path == "" {
			break
		}
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}
