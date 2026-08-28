//go:build darwin

package servicecontrol

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/testutil"
)

const darwinServiceContentCanary = "T021_PRIVATE_SERVICE_CONTENT_d937cc7e"

func TestDarwinServiceObservabilityManifestIsClosed(t *testing.T) {
	manifest, err := testutil.MergeObservabilityManifests(testutil.DarwinServiceObservabilityManifest())
	if err != nil {
		t.Fatalf("validate Darwin service observability manifest: %v", err)
	}
	want := []string{
		"host.service.darwin.stderr.crash",
		"host.service.darwin.stderr.debug",
		"host.service.darwin.stderr.failure",
		"host.service.darwin.stderr.normal",
		"host.service.darwin.stdout.crash",
		"host.service.darwin.stdout.debug",
		"host.service.darwin.stdout.failure",
		"host.service.darwin.stdout.normal",
	}
	got := make([]string, 0, len(manifest))
	for _, sink := range manifest {
		if sink.Owner != testutil.DarwinServiceObservabilityOwner {
			t.Fatalf("sink %q owner = %q, want %q", sink.ID, sink.Owner, testutil.DarwinServiceObservabilityOwner)
		}
		got = append(got, sink.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Darwin service sink IDs = %q, want %q", got, want)
	}

	manifest[0].ID = "mutated"
	if testutil.DarwinServiceObservabilityManifest()[0].ID == "mutated" {
		t.Fatal("Darwin service manifest returned shared mutable storage")
	}
}

func TestDarwinRoleDescriptorsRequireLoginStartKeepAliveAndSeparateOwnership(t *testing.T) {
	descriptors := darwinTestRoleDescriptors(t)
	if len(descriptors) != 2 {
		t.Fatalf("descriptor count = %d, want host and hub", len(descriptors))
	}
	seenLabels := make(map[string]bool)
	seenDefinitions := make(map[string]bool)
	seenPrograms := make(map[string]bool)
	for _, descriptor := range descriptors {
		if err := validateDarwinServiceDescriptor(descriptor); err != nil {
			t.Fatalf("validate %s descriptor: %v", descriptor.Role, err)
		}
		if !descriptor.RunAtLoad {
			t.Errorf("%s descriptor does not start at login", descriptor.Role)
		}
		if !descriptor.KeepAlive {
			t.Errorf("%s descriptor does not restart after unexpected exit", descriptor.Role)
		}
		if seenLabels[descriptor.Label] || seenDefinitions[descriptor.DefinitionPath] || seenPrograms[descriptor.Program] {
			t.Errorf("%s descriptor shares label, definition, or executable with another deployment role: %+v", descriptor.Role, descriptor)
		}
		seenLabels[descriptor.Label] = true
		seenDefinitions[descriptor.DefinitionPath] = true
		seenPrograms[descriptor.Program] = true
	}

	host, hub := descriptors[0], descriptors[1]
	if host.Label != "net.antst.agent-sessions" || !reflect.DeepEqual(host.ProgramArguments, []string{"daemon"}) {
		t.Fatalf("host descriptor = %+v", host)
	}
	if hub.Label != "net.antst.agent-sessions-hub" || !reflect.DeepEqual(hub.ProgramArguments, []string{"--listen", "127.0.0.1:7419"}) {
		t.Fatalf("hub descriptor = %+v", hub)
	}
	if strings.Contains(host.Program, "/hub/") || strings.Contains(hub.Program, "/host/") {
		t.Fatalf("host/hub release selection crossed ownership: host=%q hub=%q", host.Program, hub.Program)
	}
}

func TestDarwinControllerBootstrapsAndExplicitStopUsesBootout(t *testing.T) {
	for _, descriptor := range darwinTestRoleDescriptors(t) {
		t.Run(descriptor.Role, func(t *testing.T) {
			runner := &recordingDarwinRunner{}
			controller, err := newDarwinServiceController(darwinServiceControllerOptions{
				UID: 501, Runner: runner,
			})
			if err != nil {
				t.Fatalf("create Darwin controller: %v", err)
			}
			if err := controller.Start(context.Background(), descriptor); err != nil {
				t.Fatalf("bootstrap %s service: %v", descriptor.Role, err)
			}
			if err := controller.Stop(context.Background(), descriptor); err != nil {
				t.Fatalf("explicitly stop %s service: %v", descriptor.Role, err)
			}

			// A later status observation models the explicit-stop hold period. It
			// must not turn KeepAlive into launcher-driven resurrection.
			runner.queue(darwinCommandResult{ExitCode: 113, Stderr: []byte("service not found")})
			status, err := controller.Status(context.Background(), descriptor)
			if err != nil {
				t.Fatalf("observe explicitly stopped %s service: %v", descriptor.Role, err)
			}
			if status.Loaded || status.Running || status.PID != 0 {
				t.Fatalf("explicitly stopped %s status = %+v", descriptor.Role, status)
			}

			want := []recordedDarwinCommand{
				{Name: "launchctl", Args: []string{"bootstrap", "gui/501", descriptor.DefinitionPath}},
				{Name: "launchctl", Args: []string{"bootout", "gui/501/" + descriptor.Label}},
				{Name: "launchctl", Args: []string{"print", "gui/501/" + descriptor.Label}},
			}
			if got := runner.commands(); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s launchctl commands = %#v, want %#v", descriptor.Role, got, want)
			}
		})
	}
}

type recordingDarwinGenericRunner struct{ commands []recordedDarwinCommand }

func (runner *recordingDarwinGenericRunner) Run(_ context.Context, executable string, arguments ...string) error {
	runner.commands = append(runner.commands, recordedDarwinCommand{Name: executable, Args: append([]string(nil), arguments...)})
	return nil
}

func TestDarwinPersistentEnablementIsDistinctFromRuntimeStartStop(t *testing.T) {
	descriptor := darwinTestRoleDescriptors(t)[0]
	role := RoleDescriptor{
		Role: descriptor.Role, Label: descriptor.Label, DefinitionPath: descriptor.DefinitionPath,
		Program: descriptor.Program, ProgramArguments: descriptor.ProgramArguments,
	}
	runner := &recordingDarwinGenericRunner{}
	controller := NewController(runner)
	ctx := context.Background()
	for _, operation := range []func(context.Context, RoleDescriptor) error{
		controller.Enable, controller.Start, controller.Restart, controller.Stop, controller.Disable,
	} {
		if err := operation(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	want := []recordedDarwinCommand{
		{Name: "launchctl", Args: []string{"enable", domain + "/" + descriptor.Label}},
		{Name: "launchctl", Args: []string{"bootstrap", domain, descriptor.DefinitionPath}},
		{Name: "launchctl", Args: []string{"kickstart", "-k", domain + "/" + descriptor.Label}},
		{Name: "launchctl", Args: []string{"bootout", domain + "/" + descriptor.Label}},
		{Name: "launchctl", Args: []string{"disable", domain + "/" + descriptor.Label}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("Darwin service commands = %#v, want %#v", runner.commands, want)
	}
}

func TestDarwinSleepWakeObservesOneExistingOrReplacementJobWithoutLifecycleMutation(t *testing.T) {
	for _, replacementPID := range []int{7001, 7002} {
		name := "same_process"
		if replacementPID != 7001 {
			name = "launchd_replacement"
		}
		t.Run(name, func(t *testing.T) {
			descriptor := darwinTestRoleDescriptors(t)[0]
			runner := &recordingDarwinRunner{}
			runner.queue(
				darwinCommandResult{Stdout: launchdPrintFixture(descriptor.Label, 7001)},
				darwinCommandResult{Stdout: launchdPrintFixture(descriptor.Label, replacementPID)},
			)
			controller, err := newDarwinServiceController(darwinServiceControllerOptions{UID: 501, Runner: runner})
			if err != nil {
				t.Fatal(err)
			}

			before, err := controller.Status(context.Background(), descriptor)
			if err != nil {
				t.Fatalf("status before sleep: %v", err)
			}
			after, err := controller.Status(context.Background(), descriptor)
			if err != nil {
				t.Fatalf("status after wake: %v", err)
			}
			if !before.Loaded || !before.Running || before.PID != 7001 {
				t.Fatalf("pre-sleep status = %+v", before)
			}
			if !after.Loaded || !after.Running || after.PID != replacementPID {
				t.Fatalf("post-wake status = %+v", after)
			}

			commands := runner.commands()
			if len(commands) != 2 {
				t.Fatalf("sleep/wake command count = %d, want two read-only observations: %#v", len(commands), commands)
			}
			for _, command := range commands {
				if command.Name != "launchctl" || !reflect.DeepEqual(command.Args, []string{"print", "gui/501/" + descriptor.Label}) {
					t.Fatalf("sleep/wake mutated service lifecycle: %#v", commands)
				}
			}
		})
	}
}

func TestDarwinLaunchdCapturedOutputExcludesContentCanaries(t *testing.T) {
	kinds := map[string]diagnostics.OutputKind{
		"normal":  diagnostics.OutputNormal,
		"debug":   diagnostics.OutputDebug,
		"failure": diagnostics.OutputError,
		"crash":   diagnostics.OutputCrashReport,
	}
	manifest := testutil.DarwinServiceObservabilityManifest()
	covered := make(map[string]bool, len(manifest))
	for _, descriptor := range darwinTestRoleDescriptors(t) {
		for _, stream := range []string{"stdout", "stderr"} {
			path := descriptor.StandardOutputPath
			if stream == "stderr" {
				path = descriptor.StandardErrorPath
			}
			for variant, kind := range kinds {
				sinkID := "host.service.darwin." + stream + "." + variant
				body, err := diagnostics.Render(kind, "service.process", darwinServiceCanaryFields(descriptor, darwinServiceContentCanary))
				if err != nil {
					t.Fatalf("render %s %s %s capture: %v", descriptor.Role, stream, variant, err)
				}
				if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
					t.Fatalf("capture %s: %v", sinkID, err)
				}
				captured, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s capture: %v", sinkID, err)
				}
				if strings.Contains(string(captured), darwinServiceContentCanary) {
					t.Fatalf("%s %s capture leaked content canary: %s", descriptor.Role, sinkID, captured)
				}
				for _, metadata := range []string{descriptor.Role, descriptor.Label, "resource_exhausted"} {
					if !strings.Contains(string(captured), metadata) {
						t.Fatalf("%s %s capture omitted diagnostic metadata %q: %s", descriptor.Role, sinkID, metadata, captured)
					}
				}
				covered[sinkID] = true
			}
		}
	}
	for _, sink := range manifest {
		if !covered[sink.ID] {
			t.Errorf("Darwin service observability sink %q bypassed the content canary", sink.ID)
		}
	}
}

func darwinTestRoleDescriptors(t *testing.T) []darwinServiceDescriptor {
	t.Helper()
	root := t.TempDir()
	return []darwinServiceDescriptor{
		{
			Role: "host", Label: "net.antst.agent-sessions",
			DefinitionPath:   filepath.Join(root, "net.antst.agent-sessions.plist"),
			Program:          "/Users/test/.local/libexec/agent-sessions/host/current/bin/agent-sessions",
			ProgramArguments: []string{"daemon"}, RunAtLoad: true, KeepAlive: true,
			StandardOutputPath: filepath.Join(root, "host.stdout.log"),
			StandardErrorPath:  filepath.Join(root, "host.stderr.log"),
		},
		{
			Role: "hub", Label: "net.antst.agent-sessions-hub",
			DefinitionPath:   filepath.Join(root, "net.antst.agent-sessions-hub.plist"),
			Program:          "/Users/test/.local/libexec/agent-sessions/hub/current/bin/agent-sessions-hub",
			ProgramArguments: []string{"--listen", "127.0.0.1:7419"}, RunAtLoad: true, KeepAlive: true,
			StandardOutputPath: filepath.Join(root, "hub.stdout.log"),
			StandardErrorPath:  filepath.Join(root, "hub.stderr.log"),
		},
	}
}

func darwinServiceCanaryFields(descriptor darwinServiceDescriptor, canary string) map[string]any {
	return map[string]any{
		"operation": "service.lifecycle", "role": descriptor.Role, "service": descriptor.Label,
		"state": "failed", "revision": uint64(11), "error_code": "resource_exhausted",
		"message": canary + "-peer-message", "prompt": canary + "-prompt",
		"lane_input": canary + "-lane-input", "lane_result": canary + "-lane-result",
		"tool_arguments": map[string]any{"input": canary + "-tool-argument"},
		"tool_result":    canary + "-tool-result", "credential": canary + "-credential",
		"vendor_transcript": canary + "-transcript",
	}
}

func launchdPrintFixture(label string, pid int) []byte {
	return []byte(fmt.Sprintf("gui/501/%s = {\n\tactive count = 1\n\tstate = running\n\tpid = %d\n}\n", label, pid))
}

type recordedDarwinCommand struct {
	Name string
	Args []string
}

type recordingDarwinRunner struct {
	mu        sync.Mutex
	responses []darwinCommandResult
	recorded  []recordedDarwinCommand
}

func (r *recordingDarwinRunner) Run(_ context.Context, name string, args ...string) (darwinCommandResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, recordedDarwinCommand{Name: name, Args: append([]string(nil), args...)})
	if len(r.responses) == 0 {
		return darwinCommandResult{}, nil
	}
	result := r.responses[0]
	r.responses = r.responses[1:]
	return result, nil
}

func (r *recordingDarwinRunner) queue(results ...darwinCommandResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responses = append(r.responses, results...)
}

func (r *recordingDarwinRunner) commands() []recordedDarwinCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	commands := make([]recordedDarwinCommand, len(r.recorded))
	copy(commands, r.recorded)
	return commands
}
