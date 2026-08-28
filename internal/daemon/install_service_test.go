package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/servicecontrol"
)

type recordedInstalledServiceCommand struct {
	executable string
	arguments  []string
}

type recordingInstalledServiceRunner struct {
	commands []recordedInstalledServiceCommand
	err      error
}

func TestVerifyHostCandidateWaitsForCrashAmbiguousStartup(t *testing.T) {
	executable := t.TempDir() + "/agent-sessions"
	if err := os.WriteFile(executable, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := executableSHA256(executable)
	if err != nil {
		t.Fatal(err)
	}
	release := releaseinstall.InstalledRelease{Version: "1.2.3", Executable: executable}
	attempts := 0
	query := func(context.Context, string) (json.RawMessage, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("candidate socket is still starting")
		}
		return json.Marshal(HostStatusProjection{RuntimeVersion: release.Version, RuntimeIdentity: want, Generation: 1})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := verifyHostCandidateWithQuery(ctx, release, query); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("candidate readiness observations = %d, want 2 without restart redispatch", attempts)
	}
}

func TestLaunchdServiceNotFoundRequiresExactExitEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		script string
		want   bool
	}{
		{name: "exit status 113", script: "exit 113", want: true},
		{name: "exact diagnostic", script: "echo 'Could not find service' >&2; exit 1", want: true},
		{name: "permission", script: "echo 'Permission denied' >&2; exit 64"},
		{name: "transport", script: "echo 'launchd transport unavailable' >&2; exit 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := exec.Command("sh", "-c", test.script).Output()
			if err == nil {
				t.Fatal("fixture command unexpectedly succeeded")
			}
			if got := launchdServiceNotFound(err); got != test.want {
				t.Fatalf("launchdServiceNotFound(%v) = %t, want %t", err, got, test.want)
			}
		})
	}
	if launchdServiceNotFound(context.DeadlineExceeded) {
		t.Fatal("transport deadline was treated as affirmative launchd absence")
	}
}

func (runner *recordingInstalledServiceRunner) Run(_ context.Context, executable string, arguments ...string) error {
	runner.commands = append(runner.commands, recordedInstalledServiceCommand{
		executable: executable, arguments: append([]string(nil), arguments...),
	})
	return runner.err
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

func TestInstalledHostStopReobservesCrashAmbiguousAlreadyAbsentService(t *testing.T) {
	descriptor, err := HostServiceRole(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		activity string
		wantErr  bool
	}{
		{name: "absent after stop", activity: "absent"},
		{name: "inactive after stop", activity: "inactive"},
		{name: "still active", activity: "active", wantErr: true},
		{name: "unobservable", activity: "unknown", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingInstalledServiceRunner{err: errors.New("already absent or failed")}
			observed := 0
			service := &installedHostService{
				descriptor: descriptor, runner: runner, controller: servicecontrol.NewController(runner),
				activity: func(_ context.Context, manager, unit string) (string, error) {
					observed++
					wantManager, wantUnit := installedHostServiceIdentity(descriptor)
					if manager != wantManager || unit != wantUnit {
						t.Fatalf("observed service = %s/%s, want %s/%s", manager, unit, wantManager, wantUnit)
					}
					return test.activity, nil
				},
			}
			err := service.Stop(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("Stop() error = %v, wantErr=%v", err, test.wantErr)
			}
			if observed != 1 || len(runner.commands) != 1 {
				t.Fatalf("stop attempts=%d observations=%d, want one each", len(runner.commands), observed)
			}
		})
	}
}
