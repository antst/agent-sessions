package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

const (
	claudeMarketplace = "agent-sessions"
	claudePluginID    = "agent-sessions@agent-sessions"
)

type claudeConnectorDriver struct {
	runner     ConnectorCommandRunner
	executable string
	scope      string
}

func newClaudeConnectorDriver(options NativeConnectorOptions) ConnectorDriver {
	return &claudeConnectorDriver{runner: options.Runner, executable: options.ClaudeExecutable, scope: options.ClaudeScope}
}

// Prepare implements ConnectorDriver.
func (driver *claudeConnectorDriver) Prepare(ctx context.Context, request ConnectorRequest) (ConnectorMutation, error) {
	if err := validateConnectorRequest(request, "claude", filepath.Join(request.SourceRoot, "claude")); err != nil {
		return nil, err
	}
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		if err != nil {
			return nil, err
		}
		return noConnectorMutation{}, nil
	}
	prior, err := inspectClaudeConnector(ctx, driver.runner, executable, driver.scope)
	if err != nil {
		return nil, err
	}
	if prior.marketplace && (!filepath.IsAbs(prior.source) || filepath.Clean(prior.source) != prior.source) {
		return nil, errors.New("existing Claude marketplace has no recoverable absolute source")
	}
	mutation := &nativeConnectorMutation{}
	if prior.plugin {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "uninstall", "--scope", driver.scope, claudePluginID)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "install", "--scope", driver.scope, claudePluginID)
			},
		})
	}
	if prior.marketplace {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "remove", "--scope", driver.scope, claudeMarketplace)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "add", "--scope", driver.scope, prior.source)
			},
		})
	}
	mutation.steps = append(mutation.steps,
		connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "add", "--scope", driver.scope, request.SourceRoot)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "marketplace", "remove", "--scope", driver.scope, claudeMarketplace)
			},
		},
		connectorStep{
			apply: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "install", "--scope", driver.scope, claudePluginID)
			},
			undo: func(ctx context.Context) error {
				return driver.runner.Run(ctx, executable, "plugin", "uninstall", "--scope", driver.scope, claudePluginID)
			},
		},
		connectorStep{apply: func(ctx context.Context) error {
			current, inspectErr := inspectClaudeConnector(ctx, driver.runner, executable, driver.scope)
			if inspectErr != nil {
				return inspectErr
			}
			if !current.plugin || !current.marketplace || current.source != request.SourceRoot {
				return errors.New("claude did not publish the exact replacement connector")
			}
			return nil
		}},
	)
	return mutation, nil
}

// Remove implements ConnectorDriver.
func (driver *claudeConnectorDriver) Remove(ctx context.Context) error {
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		return err
	}
	prior, err := inspectClaudeConnector(ctx, driver.runner, executable, driver.scope)
	if err != nil {
		return err
	}
	if prior.plugin {
		if err := driver.runner.Run(ctx, executable, "plugin", "uninstall", "--scope", driver.scope, claudePluginID); err != nil {
			return err
		}
	}
	if prior.marketplace {
		return driver.runner.Run(ctx, executable, "plugin", "marketplace", "remove", "--scope", driver.scope, claudeMarketplace)
	}
	return nil
}

type claudeConnectorState struct {
	marketplace bool
	plugin      bool
	source      string
}

func inspectClaudeConnector(ctx context.Context, runner ConnectorCommandRunner, executable, scope string) (claudeConnectorState, error) {
	marketplacesBody, err := runner.Output(ctx, executable, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return claudeConnectorState{}, err
	}
	marketplaces, err := decodeConnectorInventory(marketplacesBody)
	if err != nil {
		return claudeConnectorState{}, err
	}
	pluginsBody, err := runner.Output(ctx, executable, "plugin", "list", "--json")
	if err != nil {
		return claudeConnectorState{}, err
	}
	plugins, err := decodeConnectorInventory(pluginsBody)
	if err != nil {
		return claudeConnectorState{}, err
	}
	state := claudeConnectorState{}
	if row, found := findConnectorObject(marketplaces, "name", claudeMarketplace); found {
		state.marketplace = true
		state.source = connectorString(row, "path", "installLocation")
	}
	for _, object := range connectorObjects(plugins) {
		if connectorString(object, "id") == claudePluginID && connectorString(object, "scope") == scope {
			state.plugin = true
			break
		}
	}
	if state.marketplace && state.source == "" {
		return claudeConnectorState{}, fmt.Errorf("claude marketplace %s has no source metadata", claudeMarketplace)
	}
	return state, nil
}
