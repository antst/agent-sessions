// Package claudeprofile prepares the minimum native Claude profile state that
// a private Agent Sessions registry needs without copying transcripts or
// credentials.
package claudeprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/antst/agent-sessions/internal/fileutil"
)

// Source describes the native Claude profile that a private attachment derives
// from. SecureConfig is deliberately allowed to be empty: on macOS that exact
// value selects Claude's ordinary, unsuffixed Keychain service while ConfigRoot
// can still be replaced with a private registry.
type Source struct {
	ConfigRoot   string
	StatePath    string
	SecureConfig string
}

// CurrentSource resolves Claude's effective native profile before Agent
// Sessions replaces CLAUDE_CONFIG_DIR with a private registry. A custom secure
// storage spelling is preserved byte-for-byte because native Claude hashes that
// spelling into its macOS Keychain service name.
func CurrentSource() (Source, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Source{}, fmt.Errorf("resolve Claude profile: %w", err)
	}
	defaultRoot := filepath.Join(home, ".claude")
	configRoot := defaultRoot
	configuredRoot := ""
	if value, exists := os.LookupEnv("CLAUDE_CONFIG_DIR"); exists && value != "" {
		configRoot = value
		configuredRoot = value
	}
	statePath := filepath.Join(home, ".claude.json")
	if configuredRoot != "" {
		statePath = filepath.Join(configuredRoot, ".claude.json")
	}
	secureConfig, secureConfigured := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if !secureConfigured {
		secureConfig = ""
		if configuredRoot != "" {
			secureConfig = configuredRoot
		}
	}
	return Source{ConfigRoot: configRoot, StatePath: statePath, SecureConfig: secureConfig}, nil
}

// SeedFromCurrent carries native account binding and first-run presentation
// state from the current effective Claude profile into privateRoot. OAuth tokens
// remain in Claude's separately configured secure storage; oauthAccount is only
// the native account metadata that tells Claude which secure credential applies.
// Presentation choices already made in the private profile continue to win.
func SeedFromCurrent(privateRoot string) error {
	source, err := CurrentSource()
	if err != nil {
		return err
	}
	return Seed(privateRoot, source.StatePath)
}

// Seed applies the private-profile state from publicPath. It is exported so
// callers can exercise the exact profile transaction without changing HOME.
func Seed(privateRoot, publicPath string) error {
	public, found, err := readProfileObject(publicPath, "Claude profile state", true)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	privatePath := filepath.Join(privateRoot, ".claude.json")
	private, found, err := readProfileObject(privatePath, "private Claude profile state", true)
	if err != nil {
		return err
	}
	if !found {
		private = map[string]any{}
	}
	changed := seedPresentation(private, public)
	accountChanged, err := syncAccount(private, public)
	if err != nil {
		return err
	}
	changed = changed || accountChanged
	if !changed {
		return nil
	}
	if err := fileutil.WriteJSONAtomic(privatePath, private); err != nil {
		return fmt.Errorf("write private Claude profile state: %w", err)
	}
	return nil
}

func readProfileObject(path, label string, allowMissing bool) (map[string]any, bool, error) {
	body, err := os.ReadFile(path) //nolint:gosec // caller supplies the current user's profile or a deterministic private profile.
	if os.IsNotExist(err) && allowMissing {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", label, err)
	}
	var profile map[string]any
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", label, err)
	}
	if profile == nil {
		return nil, false, fmt.Errorf("parse %s: expected a JSON object", label)
	}
	return profile, true, nil
}

func seedPresentation(private, public map[string]any) bool {
	changed := false
	for _, key := range []string{"hasCompletedOnboarding", "lastOnboardingVersion", "theme", "installMethod"} {
		if _, exists := private[key]; exists {
			continue
		}
		if value, exists := public[key]; exists {
			private[key] = value
			changed = true
		}
	}
	return changed
}

func syncAccount(private, public map[string]any) (bool, error) {
	// Account binding is runtime state, not a private presentation choice. Keep
	// it synchronized so account switches and logout cannot leave a private
	// attachment claiming a stale identity.
	if account, exists := public["oauthAccount"]; exists {
		if _, valid := account.(map[string]any); !valid {
			return false, errors.New("parse Claude profile state: oauthAccount must be a JSON object")
		}
		if current, present := private["oauthAccount"]; !present || !reflect.DeepEqual(current, account) {
			private["oauthAccount"] = account
			return true, nil
		}
	} else if _, exists := private["oauthAccount"]; exists {
		delete(private, "oauthAccount")
		return true, nil
	}
	return false, nil
}
