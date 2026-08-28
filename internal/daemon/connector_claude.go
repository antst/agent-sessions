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
		return unavailableConnectorMutation("claude", request.SourceRoot), nil
	}
	prior, err := inspectClaudeConnector(ctx, driver.runner, executable, driver.scope)
	if err != nil {
		return nil, err
	}
	if prior.marketplace && (!filepath.IsAbs(prior.source) || filepath.Clean(prior.source) != prior.source) {
		return nil, errors.New("existing Claude marketplace has no recoverable absolute source")
	}
	if prior.plugin && !prior.marketplace {
		return nil, errors.New("existing Claude plugin lacks its recoverable marketplace")
	}
	return newClaudeConnectorMutation(driver.runner, executable, driver.scope, request.SourceRoot, prior), nil
}

func newClaudeConnectorMutation(
	runner ConnectorCommandRunner,
	executable string,
	scope string,
	sourceRoot string,
	prior claudeConnectorState,
) *nativeConnectorMutation {
	mutation := &nativeConnectorMutation{provenance: connectorMutationProvenance{
		SchemaVersion: connectorRecoverySchemaVersion,
		Product:       "claude", Available: true, Executable: executable,
		SourceRoot: sourceRoot, PayloadRoot: filepath.Join(sourceRoot, "claude"), Scope: scope,
		Prior: connectorPriorProvenance{Marketplace: prior.marketplace, Plugin: prior.plugin, Source: prior.source},
	}}
	if prior.plugin {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return removeClaudePlugin(ctx, runner, executable, scope)
			},
			undo: func(ctx context.Context) error {
				return addClaudePlugin(ctx, runner, executable, scope)
			},
		})
	}
	if prior.marketplace {
		mutation.steps = append(mutation.steps, connectorStep{
			apply: func(ctx context.Context) error {
				return removeClaudeMarketplace(ctx, runner, executable, scope, prior.source)
			},
			undo: func(ctx context.Context) error {
				return addClaudeMarketplace(ctx, runner, executable, scope, prior.source)
			},
		})
	}
	mutation.steps = append(mutation.steps,
		connectorStep{
			apply: func(ctx context.Context) error {
				return addClaudeMarketplace(ctx, runner, executable, scope, sourceRoot)
			},
			undo: func(ctx context.Context) error {
				return removeClaudeMarketplace(ctx, runner, executable, scope, sourceRoot)
			},
		},
		connectorStep{
			apply: func(ctx context.Context) error {
				return addClaudePlugin(ctx, runner, executable, scope)
			},
			undo: func(ctx context.Context) error {
				return removeClaudePlugin(ctx, runner, executable, scope)
			},
		},
		connectorStep{apply: func(ctx context.Context) error {
			current, inspectErr := inspectClaudeConnector(ctx, runner, executable, scope)
			if inspectErr != nil {
				return inspectErr
			}
			if !current.plugin || !current.marketplace || current.source != sourceRoot {
				return errors.New("claude did not publish the exact replacement connector")
			}
			return nil
		}},
	)
	return mutation
}

func removeClaudePlugin(ctx context.Context, runner ConnectorCommandRunner, executable, scope string) error {
	current, err := inspectClaudeConnector(ctx, runner, executable, scope)
	if err != nil || !current.plugin {
		return err
	}
	return runner.Run(ctx, executable, "plugin", "uninstall", "--scope", scope, claudePluginID)
}

func addClaudePlugin(ctx context.Context, runner ConnectorCommandRunner, executable, scope string) error {
	current, err := inspectClaudeConnector(ctx, runner, executable, scope)
	if err != nil || current.plugin {
		return err
	}
	if !current.marketplace {
		return errors.New("cannot restore Claude plugin without its marketplace")
	}
	return runner.Run(ctx, executable, "plugin", "install", "--scope", scope, claudePluginID)
}

func removeClaudeMarketplace(
	ctx context.Context,
	runner ConnectorCommandRunner,
	executable string,
	scope string,
	expectedSource string,
) error {
	current, err := inspectClaudeConnector(ctx, runner, executable, scope)
	if err != nil || !current.marketplace {
		return err
	}
	if current.source != expectedSource {
		return errors.New("claude marketplace changed source during connector recovery")
	}
	return runner.Run(ctx, executable, "plugin", "marketplace", "remove", "--scope", scope, claudeMarketplace)
}

func addClaudeMarketplace(
	ctx context.Context,
	runner ConnectorCommandRunner,
	executable string,
	scope string,
	source string,
) error {
	current, err := inspectClaudeConnector(ctx, runner, executable, scope)
	if err != nil {
		return err
	}
	if current.marketplace {
		if current.source == source {
			return nil
		}
		return errors.New("claude marketplace changed source during connector recovery")
	}
	return runner.Run(ctx, executable, "plugin", "marketplace", "add", "--scope", scope, source)
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
