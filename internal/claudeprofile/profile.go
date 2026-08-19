// Package claudeprofile resolves Claude's native profile and credential
// namespace without changing either one.
package claudeprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Source describes Claude's effective shared native profile. SecureConfig is
// deliberately allowed to be empty: on macOS that exact value selects
// Claude's ordinary, unsuffixed Keychain service.
type Source struct {
	ConfigRoot     string
	StatePath      string
	SecureConfig   string
	ConfigEnvSet   bool
	ConfigEnvValue string
	SecureEnvSet   bool
}

// CurrentSource resolves Claude's effective native profile. Environment
// presence and spelling are preserved because native Claude uses them when it
// selects its macOS Keychain namespace.
func CurrentSource() (Source, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Source{}, fmt.Errorf("resolve Claude profile: %w", err)
	}
	defaultRoot := filepath.Join(home, ".claude")
	configRoot := defaultRoot
	configuredRoot, configEnvSet := os.LookupEnv("CLAUDE_CONFIG_DIR")
	if configuredRoot != "" && !filepath.IsAbs(configuredRoot) {
		return Source{}, errors.New("CLAUDE_CONFIG_DIR must be absolute for managed Claude sessions")
	}
	if configuredRoot != "" {
		configRoot = configuredRoot
	}
	if !filepath.IsAbs(configRoot) {
		return Source{}, errors.New("claude profile root must be absolute for managed Claude sessions")
	}
	statePath := filepath.Join(home, ".claude.json")
	if configuredRoot != "" {
		statePath = filepath.Join(configuredRoot, ".claude.json")
	}
	secureConfig, secureConfigured := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if secureConfig != "" && !filepath.IsAbs(secureConfig) {
		return Source{}, errors.New("CLAUDE_SECURESTORAGE_CONFIG_DIR must be absolute for managed Claude sessions")
	}
	if !secureConfigured {
		secureConfig = ""
		if configuredRoot != "" {
			secureConfig = configuredRoot
		}
	}
	return Source{
		ConfigRoot: configRoot, StatePath: statePath, SecureConfig: secureConfig,
		ConfigEnvSet: configEnvSet, ConfigEnvValue: configuredRoot, SecureEnvSet: secureConfigured,
	}, nil
}

// SharedSource resolves the exact native environment for sharedRoot. When the
// caller already selected that profile, its original env spelling (including
// explicit empty values) is preserved. A host agent configured for another
// profile makes that profile explicit without copying any state or credential.
func SharedSource(sharedRoot string) (Source, error) {
	if !filepath.IsAbs(sharedRoot) {
		return Source{}, errors.New("shared Claude profile root must be absolute")
	}
	source, err := CurrentSource()
	if err != nil {
		return Source{}, err
	}
	if sameProfilePath(source.ConfigRoot, sharedRoot) {
		return source, nil
	}
	source.ConfigRoot = sharedRoot
	source.StatePath = filepath.Join(sharedRoot, ".claude.json")
	source.ConfigEnvSet = true
	source.ConfigEnvValue = sharedRoot
	if !source.SecureEnvSet {
		source.SecureConfig = sharedRoot
	}
	return source, nil
}

func sameProfilePath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	leftResolved, leftErr := filepath.EvalSymlinks(leftAbsolute)
	rightResolved, rightErr := filepath.EvalSymlinks(rightAbsolute)
	if leftErr == nil && rightErr == nil {
		return filepath.Clean(leftResolved) == filepath.Clean(rightResolved)
	}
	return filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}
