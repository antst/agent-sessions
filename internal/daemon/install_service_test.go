package daemon

import (
	"context"
	"reflect"
	"runtime"
	"testing"

	"github.com/antst/agent-sessions/internal/servicecontrol"
)

type recordedInstalledServiceCommand struct {
	executable string
	arguments  []string
}

type recordingInstalledServiceRunner struct {
	commands []recordedInstalledServiceCommand
}

func (runner *recordingInstalledServiceRunner) Run(_ context.Context, executable string, arguments ...string) error {
	runner.commands = append(runner.commands, recordedInstalledServiceCommand{
		executable: executable, arguments: append([]string(nil), arguments...),
	})
	return nil
}

func TestInstalledLinuxHostLifecycleResetsFailureStateBeforeTransactionalStarts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd lifecycle assertion")
	}
	descriptor := servicecontrol.RoleDescriptor{Role: "host", ServiceName: "agent-sessions.service"}
	runner := &recordingInstalledServiceRunner{}
	service := &installedHostService{
		descriptor: descriptor, runner: runner, controller: servicecontrol.NewController(runner),
	}
	if err := service.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []recordedInstalledServiceCommand{
		{executable: "systemctl", arguments: []string{"--user", "daemon-reload"}},
		{executable: "systemctl", arguments: []string{"--user", "reset-failed", "agent-sessions.service"}},
		{executable: "systemctl", arguments: []string{"--user", "enable", "agent-sessions.service"}},
		{executable: "systemctl", arguments: []string{"--user", "restart", "agent-sessions.service"}},
		{executable: "systemctl", arguments: []string{"--user", "daemon-reload"}},
		{executable: "systemctl", arguments: []string{"--user", "reset-failed", "agent-sessions.service"}},
		{executable: "systemctl", arguments: []string{"--user", "start", "agent-sessions.service"}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("installed host lifecycle commands = %#v, want %#v", runner.commands, want)
	}
}
