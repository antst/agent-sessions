package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/antst/agent-sessions/internal/socketpath"
)

const (
	configurationDirectoryName = "agent-sessions"
	configurationFileName      = "config.json"
	stateDirectoryName         = "agent-sessions"
	controlSocketName          = "daemon.sock"
)

// ProductionPaths are the single canonical host-daemon paths for the current
// OS user. They are deliberately independent of cwd, TMPDIR, native product
// profiles, groups, and sessions.
type ProductionPaths struct {
	ConfigurationRoot string
	ConfigurationFile string
	StateRoot         string
	RuntimeRoot       string
	ControlEndpoint   string
}

// ResolveProductionPaths resolves the standard per-user configuration, state,
// and runtime locations. It does not create or mutate any path.
func ResolveProductionPaths() (ProductionPaths, error) {
	home, err := absoluteEnvironmentPath("HOME", "")
	if err != nil {
		return ProductionPaths{}, err
	}
	configurationBase, err := absoluteEnvironmentPath("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err != nil {
		return ProductionPaths{}, err
	}
	stateBase, err := absoluteEnvironmentPath("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	if err != nil {
		return ProductionPaths{}, err
	}
	runtimeRoot, err := platformRuntimeRoot(os.Getuid())
	if err != nil {
		return ProductionPaths{}, err
	}

	configurationRoot := filepath.Join(configurationBase, configurationDirectoryName)
	stateRoot := filepath.Join(stateBase, stateDirectoryName)
	endpoint := filepath.Join(runtimeRoot, controlSocketName)
	if err := socketpath.Validate(endpoint); err != nil {
		return ProductionPaths{}, fmt.Errorf("canonical control endpoint: %w", err)
	}
	return ProductionPaths{
		ConfigurationRoot: configurationRoot,
		ConfigurationFile: filepath.Join(configurationRoot, configurationFileName),
		StateRoot:         stateRoot,
		RuntimeRoot:       runtimeRoot,
		ControlEndpoint:   endpoint,
	}, nil
}

func productionControlEndpoint() (string, error) {
	paths, err := ResolveProductionPaths()
	if err != nil {
		return "", err
	}
	return paths.ControlEndpoint, nil
}

func absoluteEnvironmentPath(name, fallback string) (string, error) {
	value, present := os.LookupEnv(name)
	if !present || value == "" {
		value = fallback
	}
	if value == "" {
		return "", fmt.Errorf("%s must name an absolute path", name)
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must name an absolute path", name)
	}
	clean := filepath.Clean(value)
	if clean == string(filepath.Separator) {
		return "", fmt.Errorf("%s must not name the filesystem root", name)
	}
	if clean == "." || clean == "" {
		return "", errors.New(name + " is invalid")
	}
	return clean, nil
}
