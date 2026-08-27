//go:build darwin

package servicecontrol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func validatePlatformRoleDescriptor(descriptor RoleDescriptor) error {
	return validateDarwinServiceDescriptor(darwinServiceDescriptor{
		Role: descriptor.Role, Label: descriptor.Label, DefinitionPath: descriptor.DefinitionPath,
		Program: descriptor.Program, ProgramArguments: descriptor.ProgramArguments,
		RunAtLoad: true, KeepAlive: true,
	})
}

func platformServiceCommand(descriptor RoleDescriptor, operation serviceOperation) (string, []string, error) {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	switch operation {
	case serviceEnable, serviceStart:
		return "launchctl", []string{"bootstrap", domain, descriptor.DefinitionPath}, nil
	case serviceRestart:
		return "launchctl", []string{"kickstart", "-k", domain + "/" + descriptor.Label}, nil
	case serviceStop, serviceDisable:
		return "launchctl", []string{"bootout", domain + "/" + descriptor.Label}, nil
	case serviceStatus:
		return "launchctl", []string{"print", domain + "/" + descriptor.Label}, nil
	default:
		return "", nil, fmt.Errorf("unsupported launchd operation %q", operation)
	}
}

// Isolated private shape used to validate installed launchd definitions in
// Darwin tests without making the plist representation a public API.
type darwinServiceDescriptor struct {
	Role               string
	Label              string
	DefinitionPath     string
	Program            string
	ProgramArguments   []string
	RunAtLoad          bool
	KeepAlive          bool
	StandardOutputPath string
	StandardErrorPath  string
}

func validateDarwinServiceDescriptor(descriptor darwinServiceDescriptor) error {
	if descriptor.Role != "host" && descriptor.Role != "hub" {
		return fmt.Errorf("unsupported launchd role %q", descriptor.Role)
	}
	wantLabel := map[string]string{"host": "net.antst.agent-sessions", "hub": "net.antst.agent-sessions-hub"}[descriptor.Role]
	if descriptor.Label != wantLabel || strings.ContainsAny(descriptor.Label, " /\\\t\r\n") {
		return fmt.Errorf("%s launchd label %q is not canonical", descriptor.Role, descriptor.Label)
	}
	for name, path := range map[string]string{"definition": descriptor.DefinitionPath, "program": descriptor.Program} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("%s launchd %s path is not clean and absolute", descriptor.Role, name)
		}
	}
	if filepath.Ext(descriptor.DefinitionPath) != ".plist" || len(descriptor.ProgramArguments) == 0 ||
		!descriptor.RunAtLoad || !descriptor.KeepAlive {
		return errors.New("launchd descriptor requires a plist, arguments, RunAtLoad and KeepAlive")
	}
	wantReleaseComponent := string(filepath.Separator) + descriptor.Role + string(filepath.Separator)
	if !strings.Contains(descriptor.Program, wantReleaseComponent) {
		return fmt.Errorf("%s launchd program is outside its role release selection", descriptor.Role)
	}
	return nil
}

type darwinCommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type darwinCommandRunner interface {
	Run(context.Context, string, ...string) (darwinCommandResult, error)
}

type darwinServiceControllerOptions struct {
	UID    int
	Runner darwinCommandRunner
}

type darwinServiceController struct {
	uid    int
	runner darwinCommandRunner
}

type darwinServiceStatus struct {
	Loaded  bool
	Running bool
	PID     int
}

func newDarwinServiceController(options darwinServiceControllerOptions) (*darwinServiceController, error) {
	if options.UID < 0 || options.Runner == nil {
		return nil, errors.New("Darwin service controller requires a UID and runner")
	}
	return &darwinServiceController{uid: options.UID, runner: options.Runner}, nil
}

func (controller *darwinServiceController) Start(ctx context.Context, descriptor darwinServiceDescriptor) error {
	if err := validateDarwinServiceDescriptor(descriptor); err != nil {
		return err
	}
	_, err := controller.runner.Run(ctx, "launchctl", "bootstrap", controller.domain(), descriptor.DefinitionPath)
	if err != nil {
		return fmt.Errorf("bootstrap %s launchd service: %w", descriptor.Role, err)
	}
	return nil
}

func (controller *darwinServiceController) Stop(ctx context.Context, descriptor darwinServiceDescriptor) error {
	if err := validateDarwinServiceDescriptor(descriptor); err != nil {
		return err
	}
	_, err := controller.runner.Run(ctx, "launchctl", "bootout", controller.domain()+"/"+descriptor.Label)
	if err != nil {
		return fmt.Errorf("bootout %s launchd service: %w", descriptor.Role, err)
	}
	return nil
}

func (controller *darwinServiceController) Status(ctx context.Context, descriptor darwinServiceDescriptor) (darwinServiceStatus, error) {
	if err := validateDarwinServiceDescriptor(descriptor); err != nil {
		return darwinServiceStatus{}, err
	}
	result, err := controller.runner.Run(ctx, "launchctl", "print", controller.domain()+"/"+descriptor.Label)
	if err != nil {
		return darwinServiceStatus{}, fmt.Errorf("inspect %s launchd service: %w", descriptor.Role, err)
	}
	if result.ExitCode != 0 {
		return darwinServiceStatus{}, nil
	}
	status := darwinServiceStatus{Loaded: true}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") {
			status.Running = strings.TrimSpace(strings.TrimPrefix(line, "state = ")) == "running"
		}
		if strings.HasPrefix(line, "pid = ") {
			status.PID, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
		}
	}
	return status, nil
}

func (controller *darwinServiceController) domain() string {
	return "gui/" + strconv.Itoa(controller.uid)
}
