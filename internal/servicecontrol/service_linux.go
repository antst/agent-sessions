package servicecontrol

import "context"

const linuxUnit = "agent-sessions.service"

// Linux controls only the Agent Sessions systemd user unit.
type Linux struct{ runner Runner }

// NewLinux constructs a systemd user-service controller.
func NewLinux(runner Runner) *Linux { return &Linux{runner: runner} }

// Enable enables the exact user unit without changing unrelated services.
func (service *Linux) Enable(ctx context.Context) error {
	return runSteps(ctx, service.runner,
		command{operation: "reload", name: "systemctl", arguments: []string{"--user", "daemon-reload"}},
		command{operation: "enable", name: "systemctl", arguments: []string{"--user", "enable", linuxUnit}},
	)
}

// Start explicitly starts the exact user unit.
func (service *Linux) Start(ctx context.Context) error {
	return runSteps(ctx, service.runner, command{operation: "start", name: "systemctl", arguments: []string{"--user", "start", linuxUnit}})
}

// Stop explicitly stops the exact user unit. Restart=on-failure does not
// restart an explicit systemctl stop.
func (service *Linux) Stop(ctx context.Context) error {
	return runSteps(ctx, service.runner, command{operation: "stop", name: "systemctl", arguments: []string{"--user", "stop", linuxUnit}})
}

// Restart performs one validated explicit restart of the selected release.
func (service *Linux) Restart(ctx context.Context) error {
	return runSteps(ctx, service.runner, command{operation: "restart", name: "systemctl", arguments: []string{"--user", "restart", linuxUnit}})
}

// Upgrade validates the selected release before reloading and restarting. A
// validation failure leaves the currently running service untouched.
func (service *Linux) Upgrade(ctx context.Context, validate func(context.Context) error) error {
	if validate != nil {
		if err := validate(ctx); err != nil {
			return err
		}
	}
	return runSteps(ctx, service.runner,
		command{operation: "reload", name: "systemctl", arguments: []string{"--user", "daemon-reload"}},
		command{operation: "restart", name: "systemctl", arguments: []string{"--user", "restart", linuxUnit}},
	)
}

// Disable stops and disables only the exact user unit.
func (service *Linux) Disable(ctx context.Context) error {
	return runSteps(ctx, service.runner,
		command{operation: "stop", name: "systemctl", arguments: []string{"--user", "stop", linuxUnit}},
		command{operation: "disable", name: "systemctl", arguments: []string{"--user", "disable", linuxUnit}},
		command{operation: "reload", name: "systemctl", arguments: []string{"--user", "daemon-reload"}},
	)
}
