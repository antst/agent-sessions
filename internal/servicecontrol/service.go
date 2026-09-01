// Package servicecontrol owns explicit Agent Sessions user-service lifecycle
// operations. Workflow clients never receive this authority.
package servicecontrol

import (
	"context"
	"fmt"
	"os/exec"
)

// Runner executes one service-manager command and returns its bounded output.
type Runner interface {
	// Run executes one service-manager command.
	Run(context.Context, string, ...string) ([]byte, error)
}

// RunFunc adapts a function to Runner.
type RunFunc func(context.Context, string, ...string) ([]byte, error)

// Run implements Runner.
func (run RunFunc) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return run(ctx, name, arguments...)
}

// Manager exposes only explicit service-manager lifecycle operations.
type Manager interface {
	// Start starts the Agent Sessions user service.
	Start(context.Context) error
	// Stop stops the Agent Sessions user service.
	Stop(context.Context) error
	// Restart restarts the Agent Sessions user service.
	Restart(context.Context) error
}

type execRunner struct{}

// Run executes a closed-inventory service-manager command.
func (execRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput() //nolint:gosec // Closed service-manager command inventory.
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", name, err)
	}
	return output, nil
}

func runSteps(ctx context.Context, runner Runner, steps ...command) error {
	if runner == nil {
		runner = execRunner{}
	}
	for _, step := range steps {
		if _, err := runner.Run(ctx, step.name, step.arguments...); err != nil {
			return fmt.Errorf("service manager operation %s: %w", step.operation, err)
		}
	}
	return nil
}

type command struct {
	operation string
	name      string
	arguments []string
}
