package servicecontrol

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const darwinLabel = "net.antst.agent-sessions"

// Darwin controls only the Agent Sessions launchd user agent.
type Darwin struct {
	runner    Runner
	domain    string
	plistPath string
}

// NewDarwin constructs a launchd user-agent controller from the current user.
func NewDarwin(runner Runner) (*Darwin, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user home: %w", err)
	}
	return NewDarwinForUser(runner, os.Getuid(), home), nil
}

// NewDarwinForUser constructs a deterministic launchd controller for tests
// and installers that already resolved the target user.
func NewDarwinForUser(runner Runner, uid int, home string) *Darwin {
	return &Darwin{
		runner: runner, domain: fmt.Sprintf("gui/%d", uid),
		plistPath: filepath.Join(home, "Library", "LaunchAgents", darwinLabel+".plist"),
	}
}

func (service *Darwin) target() string { return service.domain + "/" + darwinLabel }

// Enable bootstraps and enables the exact launchd user agent.
func (service *Darwin) Enable(ctx context.Context) error {
	return runSteps(ctx, service.runner,
		command{operation: "bootstrap", name: "launchctl", arguments: []string{"bootstrap", service.domain, service.plistPath}},
		command{operation: "enable", name: "launchctl", arguments: []string{"enable", service.target()}},
	)
}

// Start bootstraps, enables, and kicks the exact user agent. This is the
// explicit counterpart to Stop, which boots it out of the user domain.
func (service *Darwin) Start(ctx context.Context) error {
	return runSteps(ctx, service.runner,
		command{operation: "bootstrap", name: "launchctl", arguments: []string{"bootstrap", service.domain, service.plistPath}},
		command{operation: "enable", name: "launchctl", arguments: []string{"enable", service.target()}},
		command{operation: "start", name: "launchctl", arguments: []string{"kickstart", "-k", service.target()}},
	)
}

// Stop explicitly boots out only the exact user agent.
func (service *Darwin) Stop(ctx context.Context) error {
	return runSteps(ctx, service.runner, command{operation: "stop", name: "launchctl", arguments: []string{"bootout", service.target()}})
}

// Restart explicitly replaces the exact running job instance.
func (service *Darwin) Restart(ctx context.Context) error {
	return runSteps(ctx, service.runner, command{operation: "restart", name: "launchctl", arguments: []string{"kickstart", "-k", service.target()}})
}

// Upgrade validates the selected release before replacing the loaded job. A
// validation failure leaves the currently running user agent untouched.
func (service *Darwin) Upgrade(ctx context.Context, validate func(context.Context) error) error {
	if validate != nil {
		if err := validate(ctx); err != nil {
			return err
		}
	}
	return runSteps(ctx, service.runner,
		command{operation: "bootout", name: "launchctl", arguments: []string{"bootout", service.target()}},
		command{operation: "bootstrap", name: "launchctl", arguments: []string{"bootstrap", service.domain, service.plistPath}},
		command{operation: "enable", name: "launchctl", arguments: []string{"enable", service.target()}},
		command{operation: "restart", name: "launchctl", arguments: []string{"kickstart", "-k", service.target()}},
	)
}

// Disable boots out and disables only the exact user agent.
func (service *Darwin) Disable(ctx context.Context) error {
	return runSteps(ctx, service.runner,
		command{operation: "bootout", name: "launchctl", arguments: []string{"bootout", service.target()}},
		command{operation: "disable", name: "launchctl", arguments: []string{"disable", service.target()}},
	)
}
