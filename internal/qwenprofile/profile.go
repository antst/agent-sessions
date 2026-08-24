// Package qwenprofile resolves the Qwen-owned profile and transcript
// namespace selected for a managed session.
package qwenprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/antst/agent-sessions/internal/pathidentity"
)

// LookupEnv is the presence-sensitive environment lookup used by ResolveEnvironment.
type LookupEnv func(string) (string, bool)

// Identity is the non-secret, durable identity of Qwen-owned state.
type Identity struct {
	QwenHomeSet    bool   `json:"qwen_home_set"`
	QwenHome       string `json:"qwen_home,omitempty"`
	QwenRuntimeSet bool   `json:"qwen_runtime_dir_set"`
	QwenRuntimeDir string `json:"qwen_runtime_dir,omitempty"`
	Fingerprint    string `json:"profile_fingerprint"`
}

// EffectiveHome returns the Qwen-owned directory selected by identity without
// changing whether QWEN_HOME is present in a child environment.
func EffectiveHome(identity Identity, lookup LookupEnv) (string, error) {
	if identity.QwenHomeSet {
		if identity.QwenHome == "" {
			return "", fmt.Errorf("QWEN_HOME identity is empty")
		}
		return identity.QwenHome, nil
	}
	if lookup == nil {
		return "", fmt.Errorf("qwen profile environment lookup is nil")
	}
	home, ok := lookup("HOME")
	if !ok || home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("HOME must be an absolute path when QWEN_HOME is unset")
	}
	return canonicalPath("HOME", filepath.Join(home, ".qwen"))
}

// ApplyEnvironment returns environment with the exact QWEN_HOME and
// QWEN_RUNTIME_DIR presence recorded by identity. Unrelated entries survive.
func ApplyEnvironment(environment []string, identity Identity) []string {
	filtered := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		if hasEnvironmentName(entry, "QWEN_HOME") || hasEnvironmentName(entry, "QWEN_RUNTIME_DIR") {
			continue
		}
		filtered = append(filtered, entry)
	}
	if identity.QwenHomeSet {
		filtered = append(filtered, "QWEN_HOME="+identity.QwenHome)
	}
	if identity.QwenRuntimeSet {
		filtered = append(filtered, "QWEN_RUNTIME_DIR="+identity.QwenRuntimeDir)
	}
	return filtered
}

// ResolveEnvironment resolves a profile identity without reading or mutating
// any Qwen-owned credentials, settings, plugins, or transcripts.
func ResolveEnvironment(lookup LookupEnv) (Identity, error) {
	if lookup == nil {
		return Identity{}, fmt.Errorf("qwen profile environment lookup is nil")
	}
	identity := Identity{}
	var err error
	identity.QwenHome, identity.QwenHomeSet, err = selectedPath(lookup, "QWEN_HOME")
	if err != nil {
		return Identity{}, err
	}
	identity.QwenRuntimeDir, identity.QwenRuntimeSet, err = selectedPath(lookup, "QWEN_RUNTIME_DIR")
	if err != nil {
		return Identity{}, err
	}

	// The native default is presence-sensitive too. Keep QWEN_HOME absent in
	// the launch environment, but bind its identity to the canonical HOME that
	// Qwen itself will use. This does not inspect any Qwen-owned file.
	defaultHome := ""
	if !identity.QwenHomeSet {
		home, ok := lookup("HOME")
		if !ok || home == "" || !filepath.IsAbs(home) {
			return Identity{}, fmt.Errorf("HOME must be an absolute path when QWEN_HOME is unset")
		}
		defaultHome, err = canonicalPath("HOME", filepath.Join(home, ".qwen"))
		if err != nil {
			return Identity{}, err
		}
	}

	fingerprintInput := struct {
		Version        int    `json:"version"`
		QwenHomeSet    bool   `json:"qwen_home_set"`
		QwenHome       string `json:"qwen_home,omitempty"`
		NativeHome     string `json:"native_home,omitempty"`
		QwenRuntimeSet bool   `json:"qwen_runtime_dir_set"`
		QwenRuntimeDir string `json:"qwen_runtime_dir,omitempty"`
	}{
		Version: 1, QwenHomeSet: identity.QwenHomeSet, QwenHome: identity.QwenHome,
		NativeHome: defaultHome, QwenRuntimeSet: identity.QwenRuntimeSet,
		QwenRuntimeDir: identity.QwenRuntimeDir,
	}
	body, err := json.Marshal(fingerprintInput)
	if err != nil {
		return Identity{}, fmt.Errorf("encode Qwen profile identity: %w", err)
	}
	digest := sha256.Sum256(body)
	identity.Fingerprint = hex.EncodeToString(digest[:])
	return identity, nil
}

// Current resolves the profile selected by the current process environment.
func Current() (Identity, error) {
	return ResolveEnvironment(os.LookupEnv)
}

// MatchResume requires a requested profile to exactly match the identity
// persisted for the managed session.
func MatchResume(stored, requested Identity) error {
	if stored.QwenHomeSet != requested.QwenHomeSet || stored.QwenHome != requested.QwenHome {
		return fmt.Errorf("QWEN_HOME does not match the managed Qwen session profile")
	}
	if stored.QwenRuntimeSet != requested.QwenRuntimeSet || stored.QwenRuntimeDir != requested.QwenRuntimeDir {
		return fmt.Errorf("QWEN_RUNTIME_DIR does not match the managed Qwen session profile")
	}
	if stored.Fingerprint == "" || requested.Fingerprint == "" || stored.Fingerprint != requested.Fingerprint {
		return fmt.Errorf("qwen profile fingerprint does not match the managed session")
	}
	return nil
}

func selectedPath(lookup LookupEnv, name string) (string, bool, error) {
	value, set := lookup(name)
	if !set {
		return "", false, nil
	}
	if value == "" || !filepath.IsAbs(value) {
		return "", false, fmt.Errorf("%s must be a non-empty absolute path", name)
	}
	canonical, err := canonicalPath(name, value)
	if err != nil {
		return "", false, err
	}
	return canonical, true, nil
}

// canonicalPath normalizes lexical spelling while refusing mutable symlinks in
// existing path components. Platform-owned aliases may be resolved to their
// fixed targets; missing leaves remain valid for first-run profiles.
func canonicalPath(name, value string) (string, error) {
	canonical, err := pathidentity.FuturePath(value)
	if err != nil {
		return "", fmt.Errorf("%s path: %w", name, err)
	}
	return canonical, nil
}

func hasEnvironmentName(entry, name string) bool {
	return len(entry) > len(name) && entry[:len(name)] == name && entry[len(name)] == '='
}
