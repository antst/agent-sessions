package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

const (
	codexMarketplace = "agent-sessions"
	codexPluginID    = "agent-sessions@agent-sessions"
)

type codexConnectorDriver struct {
	runner     ConnectorCommandRunner
	executable string
}

func newCodexConnectorDriver(options NativeConnectorOptions) ConnectorDriver {
	return &codexConnectorDriver{runner: options.Runner, executable: options.CodexExecutable}
}

// Prepare implements ConnectorDriver.
func (driver *codexConnectorDriver) Prepare(ctx context.Context, request ConnectorRequest) (ConnectorMutation, error) {
	if err := validateConnectorRequest(request, "codex", request.SourceRoot); err != nil {
		return nil, err
	}
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		if err != nil {
			return nil, err
		}
		return unavailableConnectorMutation("codex", request.SourceRoot), nil
	}
	prior, err := inspectCodexConnector(ctx, driver.runner, executable)
	if err != nil {
		return nil, err
	}
	if prior.marketplace && (!filepath.IsAbs(prior.source) || filepath.Clean(prior.source) != prior.source) {
		return nil, errors.New("existing Codex marketplace has no recoverable absolute source")
	}
	if prior.plugin && !prior.marketplace {
		return nil, errors.New("existing Codex plugin lacks its recoverable marketplace")
	}
	return newCodexConnectorMutation(driver.runner, executable, request.SourceRoot, prior), nil
}

func newCodexConnectorMutation(
	runner ConnectorCommandRunner,
	executable string,
	sourceRoot string,
	prior codexConnectorState,
) *nativeConnectorMutation {
	mutation := &nativeConnectorMutation{provenance: connectorMutationProvenance{
		SchemaVersion: connectorRecoverySchemaVersion,
		Product:       "codex", Available: true, Executable: executable,
		SourceRoot: sourceRoot, PayloadRoot: sourceRoot,
		Prior: connectorPriorProvenance{Marketplace: prior.marketplace, Plugin: prior.plugin, Source: prior.source},
	}}
	if prior.plugin {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return removeCodexPlugin(ctx, runner, executable)
			},
			undo: func(ctx context.Context) error {
				return addCodexPlugin(ctx, runner, executable)
			},
		})
	}
	if prior.marketplace {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return removeCodexMarketplace(ctx, runner, executable, prior.source)
			},
			undo: func(ctx context.Context) error {
				return addCodexMarketplace(ctx, runner, executable, prior.source)
			},
		})
	}
	mutation.steps = append(mutation.steps,
		connectorStep{
			apply: func(ctx context.Context) error {
				return addCodexMarketplace(ctx, runner, executable, sourceRoot)
			},
			undo: func(ctx context.Context) error {
				return removeCodexMarketplace(ctx, runner, executable, sourceRoot)
			},
		},
		connectorStep{
			apply: func(ctx context.Context) error {
				return addCodexPlugin(ctx, runner, executable)
			},
			undo: func(ctx context.Context) error {
				return removeCodexPlugin(ctx, runner, executable)
			},
		},
		connectorStep{apply: func(ctx context.Context) error {
			current, inspectErr := inspectCodexConnector(ctx, runner, executable)
			if inspectErr != nil {
				return inspectErr
			}
			if !current.plugin || !current.marketplace || current.source != sourceRoot {
				return errors.New("codex did not publish the exact replacement connector")
			}
			return nil
		}},
	)
	return mutation
}

func removeCodexPlugin(ctx context.Context, runner ConnectorCommandRunner, executable string) error {
	current, err := inspectCodexConnector(ctx, runner, executable)
	if err != nil || !current.plugin {
		return err
	}
	return runner.Run(ctx, executable, "plugin", "remove", codexPluginID)
}

func addCodexPlugin(ctx context.Context, runner ConnectorCommandRunner, executable string) error {
	current, err := inspectCodexConnector(ctx, runner, executable)
	if err != nil || current.plugin {
		return err
	}
	if !current.marketplace {
		return errors.New("cannot restore Codex plugin without its marketplace")
	}
	return runner.Run(ctx, executable, "plugin", "add", codexPluginID)
}

func removeCodexMarketplace(
	ctx context.Context,
	runner ConnectorCommandRunner,
	executable string,
	expectedSource string,
) error {
	current, err := inspectCodexConnector(ctx, runner, executable)
	if err != nil || !current.marketplace {
		return err
	}
	if current.source != expectedSource {
		return errors.New("codex marketplace changed source during connector recovery")
	}
	return runner.Run(ctx, executable, "plugin", "marketplace", "remove", codexMarketplace)
}

func addCodexMarketplace(
	ctx context.Context,
	runner ConnectorCommandRunner,
	executable string,
	source string,
) error {
	current, err := inspectCodexConnector(ctx, runner, executable)
	if err != nil {
		return err
	}
	if current.marketplace {
		if current.source == source {
			return nil
		}
		return errors.New("codex marketplace changed source during connector recovery")
	}
	return runner.Run(ctx, executable, "plugin", "marketplace", "add", source)
}

// Remove implements ConnectorDriver.
func (driver *codexConnectorDriver) Remove(ctx context.Context) error {
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		return err
	}
	prior, err := inspectCodexConnector(ctx, driver.runner, executable)
	if err != nil {
		return err
	}
	if prior.plugin {
		if err := driver.runner.Run(ctx, executable, "plugin", "remove", codexPluginID); err != nil {
			return err
		}
	}
	if prior.marketplace {
		return driver.runner.Run(ctx, executable, "plugin", "marketplace", "remove", codexMarketplace)
	}
	return nil
}

type codexConnectorState struct {
	marketplace bool
	plugin      bool
	source      string
}

func inspectCodexConnector(ctx context.Context, runner ConnectorCommandRunner, executable string) (codexConnectorState, error) {
	marketplacesBody, err := runner.Output(ctx, executable, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return codexConnectorState{}, err
	}
	marketplaces, err := decodeConnectorInventory(marketplacesBody)
	if err != nil {
		return codexConnectorState{}, err
	}
	pluginsBody, err := runner.Output(ctx, executable, "plugin", "list", "--json")
	if err != nil {
		return codexConnectorState{}, err
	}
	plugins, err := decodeConnectorInventory(pluginsBody)
	if err != nil {
		return codexConnectorState{}, err
	}
	state := codexConnectorState{}
	if row, found := findConnectorObject(marketplaces, "name", codexMarketplace); found {
		state.marketplace = true
		state.source = connectorString(row, "root", "path", "source")
		if nested, ok := row["marketplaceSource"].(map[string]any); ok {
			if source := connectorString(nested, "source", "path"); source != "" {
				state.source = source
			}
		}
	}
	_, state.plugin = findConnectorObject(plugins, "pluginId", codexPluginID)
	if state.marketplace && state.source == "" {
		return codexConnectorState{}, fmt.Errorf("codex marketplace %s has no source metadata", codexMarketplace)
	}
	return state, nil
}
