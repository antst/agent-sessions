// Package servicecontrol provides descriptor-driven user service-manager
// operations shared by the host and hub deployment roles.
package servicecontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// RoleDescriptor identifies one independently managed service role.
type RoleDescriptor struct {
	Role             string
	ServiceName      string
	Label            string
	DefinitionPath   string
	Program          string
	ProgramArguments []string
}

// CommandRunner executes one platform service-manager command.
type CommandRunner interface {
	// Run invokes one descriptor-derived service-manager operation.
	Run(context.Context, string, ...string) error
}

// Controller performs lifecycle operations without owning service lifetime.
type Controller struct{ runner CommandRunner }

// OSCommandRunner invokes the platform service manager without exposing its
// potentially unbounded output through the lifecycle API.
type OSCommandRunner struct{}

// Run implements CommandRunner.
func (OSCommandRunner) Run(ctx context.Context, executable string, arguments ...string) error {
	command := exec.CommandContext(ctx, executable, arguments...) //nolint:gosec // Executable and argv come only from validated role descriptors.
	command.Stdin, command.Stdout, command.Stderr = nil, io.Discard, io.Discard
	return command.Run()
}

// Status is one read-only service-manager observation.
type Status struct {
	Loaded  bool
	Running bool
}

// NewController creates a service controller backed by runner.
func NewController(runner CommandRunner) *Controller { return &Controller{runner: runner} }

// Enable configures login start without implicitly starting the service.
func (controller *Controller) Enable(ctx context.Context, descriptor RoleDescriptor) error {
	return controller.run(ctx, descriptor, serviceEnable)
}

// Start explicitly starts an already installed service.
func (controller *Controller) Start(ctx context.Context, descriptor RoleDescriptor) error {
	return controller.run(ctx, descriptor, serviceStart)
}

// Restart asks the service manager for exactly one restart transaction.
func (controller *Controller) Restart(ctx context.Context, descriptor RoleDescriptor) error {
	return controller.run(ctx, descriptor, serviceRestart)
}

// Stop explicitly stops the service through its persistent platform operation.
func (controller *Controller) Stop(ctx context.Context, descriptor RoleDescriptor) error {
	return controller.run(ctx, descriptor, serviceStop)
}

// Disable suppresses login start without touching another deployment role.
func (controller *Controller) Disable(ctx context.Context, descriptor RoleDescriptor) error {
	return controller.run(ctx, descriptor, serviceDisable)
}

// Status observes the configured service without starting or repairing it.
func (controller *Controller) Status(ctx context.Context, descriptor RoleDescriptor) (Status, error) {
	err := controller.run(ctx, descriptor, serviceStatus)
	if err != nil {
		return Status{}, err
	}
	return Status{Loaded: true, Running: true}, nil
}

type serviceOperation string

const (
	serviceEnable  serviceOperation = "enable"
	serviceStart   serviceOperation = "start"
	serviceRestart serviceOperation = "restart"
	serviceStop    serviceOperation = "stop"
	serviceDisable serviceOperation = "disable"
	serviceStatus  serviceOperation = "status"
)

func (controller *Controller) run(ctx context.Context, descriptor RoleDescriptor, operation serviceOperation) error {
	if controller == nil || controller.runner == nil {
		return errors.New("service controller requires a command runner")
	}
	if err := validateRoleDescriptor(descriptor); err != nil {
		return err
	}
	executable, arguments, err := platformServiceCommand(descriptor, operation)
	if err != nil {
		return err
	}
	if err := controller.runner.Run(ctx, executable, arguments...); err != nil {
		return fmt.Errorf("%s %s service %s: %w", operation, descriptor.Role, descriptorIdentity(descriptor), err)
	}
	return nil
}

func validateRoleDescriptor(descriptor RoleDescriptor) error {
	if descriptor.Role != "host" && descriptor.Role != "hub" {
		return fmt.Errorf("unsupported service role %q", descriptor.Role)
	}
	if descriptor.Role != filepath.Base(descriptor.Role) || strings.ContainsAny(descriptor.Role, `/\`) {
		return fmt.Errorf("service role %q is not canonical", descriptor.Role)
	}
	return validatePlatformRoleDescriptor(descriptor)
}

func descriptorIdentity(descriptor RoleDescriptor) string {
	if descriptor.ServiceName != "" {
		return descriptor.ServiceName
	}
	return descriptor.Label
}
