package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const (
	// DaemonConfigSchemaVersion is the only accepted host configuration schema.
	DaemonConfigSchemaVersion = 1
	maxDaemonConfigBytes      = 1024 * 1024
)

// ProductOverride selects non-secret native resources for one product. The
// values are passed to the native adapter; Agent Sessions never interprets
// them as another daemon identity or runtime namespace.
type ProductOverride struct {
	Executable string `json:"executable,omitempty"`
	Profile    string `json:"profile,omitempty"`
}

// DaemonConfig is the canonical non-secret host configuration.
//
//nolint:revive // DaemonConfig is the contract name used throughout the specification.
type DaemonConfig struct {
	SchemaVersion      int                        `json:"schema_version"`
	HostID             string                     `json:"host_id"`
	HostName           string                     `json:"host_name"`
	HubAddress         string                     `json:"hub_address,omitempty"`
	RemoteLanesEnabled bool                       `json:"remote_lanes_enabled"`
	ProductOverrides   map[string]ProductOverride `json:"product_overrides,omitempty"`
	StateRoot          string                     `json:"state_root"`
	RuntimeRoot        string                     `json:"runtime_root"`
	Revision           uint64                     `json:"revision"`
	UpdatedAt          int64                      `json:"updated_at"`
}

// Validate checks the complete closed, non-secret host configuration contract.
//
//nolint:gocyclo // The closed configuration schema keeps every rejection explicit and auditable.
func (configuration DaemonConfig) Validate(paths ProductionPaths) error {
	if configuration.SchemaVersion != DaemonConfigSchemaVersion {
		return fmt.Errorf("unsupported daemon configuration schema %d", configuration.SchemaVersion)
	}
	if strings.TrimSpace(configuration.HostID) == "" || strings.TrimSpace(configuration.HostName) == "" {
		return errors.New("daemon configuration requires host_id and host_name")
	}
	if configuration.HostID != strings.TrimSpace(configuration.HostID) ||
		configuration.HostName != strings.TrimSpace(configuration.HostName) {
		return errors.New("daemon host identity must be canonical")
	}
	if configuration.StateRoot != paths.StateRoot || configuration.RuntimeRoot != paths.RuntimeRoot {
		return errors.New("daemon configuration paths do not match the canonical user roots")
	}
	if configuration.Revision == 0 || configuration.UpdatedAt <= 0 {
		return errors.New("daemon configuration requires a positive revision and updated_at")
	}
	for product, override := range configuration.ProductOverrides {
		if _, ok := productcatalog.ProductByID(product); !ok {
			return fmt.Errorf("daemon configuration contains unknown product %q", product)
		}
		if override.Executable != "" && filepath.Base(override.Executable) != override.Executable && !filepath.IsAbs(override.Executable) {
			return fmt.Errorf("product %q executable must be a basename or absolute path", product)
		}
		if override.Profile != "" && !filepath.IsAbs(override.Profile) {
			return fmt.Errorf("product %q profile must be an absolute path", product)
		}
	}
	return nil
}

// LoadDaemonConfig reads one bounded regular configuration file and rejects
// unknown fields. Credential values are neither required nor inspected.
func LoadDaemonConfig(path string, paths ProductionPaths) (DaemonConfig, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return DaemonConfig{}, errors.New("daemon configuration path must be clean and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return DaemonConfig{}, fmt.Errorf("inspect daemon configuration: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return DaemonConfig{}, errors.New("daemon configuration is not a real regular file")
	}
	if info.Size() < 1 || info.Size() > maxDaemonConfigBytes {
		return DaemonConfig{}, errors.New("daemon configuration is empty or unbounded")
	}
	file, err := os.Open(path) //nolint:gosec // validated canonical caller-owned configuration path.
	if err != nil {
		return DaemonConfig{}, fmt.Errorf("open daemon configuration: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxDaemonConfigBytes+1))
	if err != nil {
		return DaemonConfig{}, fmt.Errorf("read daemon configuration: %w", err)
	}
	if len(body) > maxDaemonConfigBytes {
		return DaemonConfig{}, errors.New("daemon configuration exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var configuration DaemonConfig
	if err := decoder.Decode(&configuration); err != nil {
		return DaemonConfig{}, fmt.Errorf("decode daemon configuration: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return DaemonConfig{}, errors.New("daemon configuration contains trailing JSON")
	}
	if err := configuration.Validate(paths); err != nil {
		return DaemonConfig{}, err
	}
	return configuration, nil
}
