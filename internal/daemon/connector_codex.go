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
		return noConnectorMutation{}, nil
	}
	prior, err := inspectCodexConnector(ctx, driver.runner, executable)
	if err != nil {
		return nil, err
	}
	if prior.marketplace && (!filepath.IsAbs(prior.source) || filepath.Clean(prior.source) != prior.source) {
		return nil, errors.New("existing Codex marketplace has no recoverable absolute source")
	}
	mutation := &nativeConnectorMutation{}
	if prior.plugin {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "remove", codexPluginID)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "add", codexPluginID)
			},
		})
	}
	if prior.marketplace {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "remove", codexMarketplace)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "add", prior.source)
			},
		})
	}
	mutation.steps = append(mutation.steps,
		connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "add", request.SourceRoot)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "remove", codexMarketplace)
			},
		},
		connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "add", codexPluginID)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "remove", codexPluginID)
			},
		},
		connectorStep{apply: func(ctx context.Context) error {
			current, inspectErr := inspectCodexConnector(ctx, driver.runner, executable)
			if inspectErr != nil {
				return inspectErr
			}
			if !current.plugin || !current.marketplace || current.source != request.SourceRoot {
				return errors.New("codex did not publish the exact replacement connector")
			}
			return nil
		}},
	)
	return mutation, nil
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
