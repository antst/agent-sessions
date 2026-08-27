package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const qwenConnectorName = "agent-sessions"

type qwenConnectorDriver struct {
	runner     ConnectorCommandRunner
	executable string
}

func newQwenConnectorDriver(options NativeConnectorOptions) ConnectorDriver {
	return &qwenConnectorDriver{runner: options.Runner, executable: options.QwenExecutable}
}

// Prepare implements ConnectorDriver.
//
//nolint:gocyclo // Preparation keeps exact prior-source, policy, and rollback validation at one boundary.
func (driver *qwenConnectorDriver) Prepare(_ context.Context, request ConnectorRequest) (ConnectorMutation, error) {
	payloadRoot := filepath.Join(request.SourceRoot, "qwen")
	if err := validateConnectorRequest(request, "qwen", payloadRoot); err != nil {
		return nil, err
	}
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		if err != nil {
			return nil, err
		}
		return noConnectorMutation{}, nil
	}
	version, err := qwenConnectorVersion(payloadRoot)
	if err != nil {
		return nil, err
	}
	profile, err := qwenprofile.Current()
	if err != nil {
		return nil, err
	}
	home, err := qwenprofile.EffectiveHome(profile, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	prior, err := inspectQwenConnector(home)
	if err != nil {
		return nil, err
	}
	if prior.present {
		if !filepath.IsAbs(prior.source) || filepath.Clean(prior.source) != prior.source {
			return nil, errors.New("existing Qwen extension has no recoverable absolute source")
		}
		if _, err := qwenConnectorVersion(prior.source); err != nil {
			return nil, fmt.Errorf("existing Qwen extension source is not recoverable: %w", err)
		}
	}
	mutation := &nativeConnectorMutation{}
	if prior.present {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "extensions", "uninstall", qwenConnectorName)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "extensions", "install", prior.source, "--scope", "user", "--consent")
			},
		})
	}
	mutation.steps = append(mutation.steps,
		connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "extensions", "install", payloadRoot, "--scope", "user", "--consent")
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "extensions", "uninstall", qwenConnectorName)
			},
		},
		connectorStep{apply: func(context.Context) error {
			current, inspectErr := inspectQwenConnector(home)
			if inspectErr != nil {
				return inspectErr
			}
			if !current.present || !current.enabled || current.version != version || !sameConnectorSource(current.source, payloadRoot) {
				return errors.New("qwen did not publish the exact replacement connector")
			}
			return nil
		}},
	)
	return mutation, nil
}

// Remove implements ConnectorDriver.
func (driver *qwenConnectorDriver) Remove(ctx context.Context) error {
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		return err
	}
	profile, err := qwenprofile.Current()
	if err != nil {
		return err
	}
	home, err := qwenprofile.EffectiveHome(profile, os.LookupEnv)
	if err != nil {
		return err
	}
	present, err := inspectQwenConnectorRemoval(home)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return driver.runner.Run(ctx, executable, "extensions", "uninstall", qwenConnectorName)
}

func inspectQwenConnectorRemoval(home string) (bool, error) {
	root := filepath.Join(home, "extensions", qwenConnectorName)
	info, rootErr := os.Lstat(root)
	rootPresent := rootErr == nil
	if rootPresent && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return false, errors.New("qwen connector root changed filesystem type")
	}
	if rootErr != nil && !errors.Is(rootErr, os.ErrNotExist) {
		return false, rootErr
	}
	statePath := filepath.Join(home, "extension-store", "state.json")
	state, stateErr := readConnectorJSONObject(statePath)
	if errors.Is(stateErr, os.ErrNotExist) {
		stateErr = nil
	}
	if stateErr != nil {
		return false, fmt.Errorf("read Qwen extension policy metadata: %w", stateErr)
	}
	matches := 0
	if state != nil {
		extensions, _ := state["extensions"].(map[string]any)
		for _, object := range extensions {
			policy, _ := object.(map[string]any)
			if connectorString(policy, "name") == qwenConnectorName {
				matches++
			}
		}
	}
	if matches > 1 || (rootPresent != (matches == 1)) {
		return false, errors.New("qwen connector payload and policy presence disagree")
	}
	return rootPresent, nil
}

type qwenConnectorState struct {
	present bool
	enabled bool
	source  string
	version string
}

func inspectQwenConnector(home string) (qwenConnectorState, error) {
	root := filepath.Join(home, "extensions", qwenConnectorName)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return qwenConnectorState{}, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return qwenConnectorState{}, errors.New("qwen connector root is not a real directory")
	}
	version, err := qwenConnectorVersion(root)
	if err != nil {
		return qwenConnectorState{}, err
	}
	install, err := readConnectorJSONObject(filepath.Join(root, ".qwen-extension-install.json"))
	if err != nil {
		return qwenConnectorState{}, fmt.Errorf("read Qwen connector source metadata: %w", err)
	}
	source := connectorString(install, "source")
	if source == "" {
		return qwenConnectorState{}, errors.New("qwen connector source metadata is empty")
	}
	state, err := readConnectorJSONObject(filepath.Join(home, "extension-store", "state.json"))
	if err != nil {
		return qwenConnectorState{}, fmt.Errorf("read Qwen extension policy metadata: %w", err)
	}
	enabled, matches := false, 0
	extensions, _ := state["extensions"].(map[string]any)
	for _, object := range extensions {
		policy, _ := object.(map[string]any)
		if connectorString(policy, "name") != qwenConnectorName {
			continue
		}
		matches++
		enabled = connectorString(policy, "defaultActivation") == "enabled"
	}
	if matches != 1 {
		return qwenConnectorState{}, fmt.Errorf("qwen extension store contains %d agent-sessions policies", matches)
	}
	return qwenConnectorState{present: true, enabled: enabled, source: source, version: version}, nil
}

func qwenConnectorVersion(root string) (string, error) {
	manifest, err := readConnectorJSONObject(filepath.Join(root, "plugin.json"))
	if err != nil {
		return "", err
	}
	version := connectorString(manifest, "version")
	if connectorString(manifest, "name") != qwenConnectorName || version == "" {
		return "", errors.New("qwen connector manifest has invalid identity or version")
	}
	return version, nil
}

func readConnectorJSONObject(path string) (map[string]any, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 2*1024*1024 {
		return nil, errors.New("connector metadata is not a bounded regular file")
	}
	body, err := os.ReadFile(path) //nolint:gosec // Exact non-secret connector manifest or vendor extension inventory metadata.
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func sameConnectorSource(left, right string) bool {
	leftPath, leftErr := filepath.EvalSymlinks(left)
	rightPath, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && leftPath == rightPath
}
