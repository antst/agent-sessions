//go:build darwin

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/servicecontrol"
)

func TestObserveInstalledLaunchdServiceRejectsLoadedJobWithoutDefinition(t *testing.T) {
	bin := t.TempDir()
	launchctl := filepath.Join(bin, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\nif [ \"$1\" = print ]; then echo 'state = running'; echo 'pid = 4242'; exit 0; fi\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	descriptor := servicecontrol.RoleDescriptor{
		Role: "host", Label: "net.antst.agent-sessions",
		DefinitionPath:   filepath.Join(t.TempDir(), "missing.plist"),
		Program:          filepath.Join(t.TempDir(), "host", "current", "bin", "agent-sessions"),
		ProgramArguments: []string{"daemon"},
	}
	if _, err := observeInstalledLaunchdService(context.Background(), descriptor); err == nil {
		t.Fatal("loaded launchd job without its exact definition was snapshotted as absent")
	}
}

func TestObserveInstalledLaunchdServiceDistinguishesNotFoundFromObservationFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "exact exit status", body: "exit 113\n"},
		{name: "exact diagnostic", body: "echo 'Could not find service' >&2\nexit 1\n"},
		{name: "permission failure", body: "echo 'Permission denied' >&2\nexit 64\n", wantErr: true},
		{name: "transport failure", body: "echo 'launchd transport unavailable' >&2\nexit 1\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			launchctl := filepath.Join(bin, "launchctl")
			script := "#!/bin/sh\nif [ \"$1\" = print ]; then\n" + test.body + "fi\nexit 0\n"
			if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			descriptor := servicecontrol.RoleDescriptor{
				Role: "host", Label: "net.antst.agent-sessions",
				DefinitionPath:   filepath.Join(t.TempDir(), "missing.plist"),
				Program:          filepath.Join(t.TempDir(), "host", "current", "bin", "agent-sessions"),
				ProgramArguments: []string{"daemon"},
			}
			_, err := observeInstalledLaunchdService(context.Background(), descriptor)
			if test.wantErr && (err == nil || !strings.Contains(err.Error(), "observe host launchd runtime")) {
				t.Fatalf("observation failure = %v, want fail-closed runtime error", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("exact launchd absence was rejected: %v", err)
			}
		})
	}
}
