package grok

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func sessionArtifactDirectory(environment []string, cwd string) (string, error) {
	base := environmentValue(environment, "GROK_HOME")
	if base == "" {
		home := environmentValue(environment, "HOME")
		var err error
		if home == "" {
			home, err = os.UserHomeDir()
			if err != nil {
				return "", err
			}
		}
		base = filepath.Join(home, ".grok")
	}
	encoded := url.PathEscape(cwd)
	if len(encoded) > 255 {
		return "", fmt.Errorf("Grok session artifact name for this cwd is %d bytes, over the 255-byte limit; run from a shorter path", len(encoded))
	}
	return filepath.Join(base, "sessions", encoded), nil
}

func sessionArtifact(environment []string, cwd, id string) (string, bool, time.Time, error) {
	directory, err := sessionArtifactDirectory(environment, cwd)
	if err != nil {
		return "", false, time.Time{}, err
	}
	path := filepath.Join(directory, id)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return path, false, time.Time{}, nil
	}
	if err != nil {
		return "", false, time.Time{}, err
	}
	if !info.IsDir() {
		return "", false, time.Time{}, fmt.Errorf("Grok session artifact is not a directory: %s", path)
	}
	return path, true, info.ModTime(), nil
}

func sessionArtifactReady(path string, existed bool, before time.Time) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("Grok session artifact is not a directory: %s", path)
	}
	return !existed || !info.ModTime().Equal(before), nil
}
