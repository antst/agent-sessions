//go:build linux

package servicecontrol

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/testutil"
)

const linuxServiceContentCanary = "T020_SYSTEMD_PRIVATE_CONTENT_49e608b7"

type recordedServiceCommand struct {
	executable string
	arguments  []string
}

type recordingServiceRunner struct {
	commands []recordedServiceCommand
	err      error
}

func (r *recordingServiceRunner) Run(_ context.Context, executable string, arguments ...string) error {
	r.commands = append(r.commands, recordedServiceCommand{
		executable: executable,
		arguments:  append([]string(nil), arguments...),
	})
	return r.err
}

func TestLinuxServiceLifecycleUsesExactRoleDescriptor(t *testing.T) {
	descriptors := []RoleDescriptor{
		{Role: "host", ServiceName: "agent-sessions.service"},
		{Role: "hub", ServiceName: "agent-sessions-hub.service"},
	}
	operations := []struct {
		name   string
		verb   string
		invoke func(context.Context, *Controller, RoleDescriptor) error
	}{
		{name: "enable", verb: "enable", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Enable(ctx, descriptor)
		}},
		{name: "start", verb: "start", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Start(ctx, descriptor)
		}},
		{name: "restart", verb: "restart", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Restart(ctx, descriptor)
		}},
		{name: "stop", verb: "stop", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Stop(ctx, descriptor)
		}},
		{name: "disable", verb: "disable", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Disable(ctx, descriptor)
		}},
	}

	for _, descriptor := range descriptors {
		for _, operation := range operations {
			t.Run(descriptor.Role+"/"+operation.name, func(t *testing.T) {
				runner := &recordingServiceRunner{}
				controller := NewController(runner)
				if err := operation.invoke(context.Background(), controller, descriptor); err != nil {
					t.Fatalf("%s %s service: %v", operation.name, descriptor.Role, err)
				}
				want := []recordedServiceCommand{{
					executable: "systemctl",
					arguments:  []string{"--user", operation.verb, descriptor.ServiceName},
				}}
				if !reflect.DeepEqual(runner.commands, want) {
					t.Fatalf("commands = %#v, want %#v", runner.commands, want)
				}
			})
		}
	}
}

func TestLinuxEnableEstablishesLoginStartWithoutStartingService(t *testing.T) {
	runner := &recordingServiceRunner{}
	controller := NewController(runner)
	descriptor := RoleDescriptor{Role: "host", ServiceName: "agent-sessions.service"}

	if err := controller.Enable(context.Background(), descriptor); err != nil {
		t.Fatalf("enable login start: %v", err)
	}
	want := make([]recordedServiceCommand, 0, 2)
	want = append(want, recordedServiceCommand{
		executable: "systemctl",
		arguments:  []string{"--user", "enable", "agent-sessions.service"},
	})
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("enable commands = %#v, want login enable only %#v", runner.commands, want)
	}

	if err := controller.Start(context.Background(), descriptor); err != nil {
		t.Fatalf("explicit start: %v", err)
	}
	want = append(want, recordedServiceCommand{
		executable: "systemctl",
		arguments:  []string{"--user", "start", "agent-sessions.service"},
	})
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("enable then start commands = %#v, want %#v", runner.commands, want)
	}
}

func TestLinuxServiceDescriptorValidationFailsBeforeSystemctl(t *testing.T) {
	tests := []struct {
		name       string
		descriptor RoleDescriptor
	}{
		{name: "missing role", descriptor: RoleDescriptor{ServiceName: "agent-sessions.service"}},
		{name: "unknown role", descriptor: RoleDescriptor{Role: "sidecar", ServiceName: "agent-sessions.service"}},
		{name: "role is a path", descriptor: RoleDescriptor{Role: "host/../hub", ServiceName: "agent-sessions.service"}},
		{name: "missing unit", descriptor: RoleDescriptor{Role: "host"}},
		{name: "unit is not a service", descriptor: RoleDescriptor{Role: "host", ServiceName: "agent-sessions"}},
		{name: "unit is a path", descriptor: RoleDescriptor{Role: "host", ServiceName: "../agent-sessions.service"}},
		{name: "unit contains arguments", descriptor: RoleDescriptor{Role: "host", ServiceName: "agent-sessions.service --now"}},
	}
	operations := []struct {
		name   string
		invoke func(context.Context, *Controller, RoleDescriptor) error
	}{
		{name: "enable", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Enable(ctx, descriptor)
		}},
		{name: "start", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Start(ctx, descriptor)
		}},
		{name: "restart", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Restart(ctx, descriptor)
		}},
		{name: "stop", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Stop(ctx, descriptor)
		}},
		{name: "disable", invoke: func(ctx context.Context, controller *Controller, descriptor RoleDescriptor) error {
			return controller.Disable(ctx, descriptor)
		}},
	}

	for _, test := range tests {
		for _, operation := range operations {
			t.Run(test.name+"/"+operation.name, func(t *testing.T) {
				runner := &recordingServiceRunner{}
				controller := NewController(runner)
				if err := operation.invoke(context.Background(), controller, test.descriptor); err == nil {
					t.Fatalf("%s accepted invalid descriptor %#v", operation.name, test.descriptor)
				}
				if len(runner.commands) != 0 {
					t.Fatalf("invalid descriptor reached systemctl: %#v", runner.commands)
				}
			})
		}
	}
}

func TestLinuxServiceRunnerFailureIsAttributedToExactRoleAndOperation(t *testing.T) {
	runner := &recordingServiceRunner{err: errors.New("systemctl unavailable")}
	controller := NewController(runner)
	descriptor := RoleDescriptor{Role: "hub", ServiceName: "agent-sessions-hub.service"}
	err := controller.Restart(context.Background(), descriptor)
	if err == nil {
		t.Fatal("restart unexpectedly succeeded")
	}
	for _, want := range []string{"hub", "restart", "agent-sessions-hub.service"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("restart error %q omitted exact attribution %q", err, want)
		}
	}
}

func TestLinuxServiceObservabilityManifestIsClosedAndComplete(t *testing.T) {
	manifest, err := testutil.MergeObservabilityManifests(testutil.LinuxServiceObservabilityManifest())
	if err != nil {
		t.Fatalf("validate Linux service observability manifest: %v", err)
	}
	got := make([]string, 0, len(manifest))
	for _, sink := range manifest {
		if sink.Owner != testutil.LinuxServiceObservabilityOwner {
			t.Fatalf("sink %q owner = %q, want %q", sink.ID, sink.Owner, testutil.LinuxServiceObservabilityOwner)
		}
		got = append(got, sink.ID)
	}
	want := []string{
		"linux.systemd.journal.crash",
		"linux.systemd.journal.debug",
		"linux.systemd.journal.failure",
		"linux.systemd.journal.normal",
		"linux.systemd.stderr.crash",
		"linux.systemd.stderr.debug",
		"linux.systemd.stderr.failure",
		"linux.systemd.stderr.normal",
		"linux.systemd.stdout.crash",
		"linux.systemd.stdout.debug",
		"linux.systemd.stdout.failure",
		"linux.systemd.stdout.normal",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Linux service sink IDs = %q, want %q", got, want)
	}
	applicable, err := testutil.MergeObservabilityManifests(
		testutil.HostCoreObservabilityManifest(),
		testutil.LinuxServiceObservabilityManifest(),
	)
	if err != nil {
		t.Fatalf("merge applicable Linux host observability manifest: %v", err)
	}
	if wantCount := len(testutil.HostCoreObservabilityManifest()) + len(want); len(applicable) != wantCount {
		t.Fatalf("applicable Linux observability sinks = %d, want %d", len(applicable), wantCount)
	}

	first := testutil.LinuxServiceObservabilityManifest()
	first[0].ID = "mutated"
	if second := testutil.LinuxServiceObservabilityManifest(); second[0].ID == "mutated" {
		t.Fatal("Linux service manifest returned shared mutable storage")
	}
}

func TestEveryLinuxServiceCaptureDelegatesToSharedContentCanary(t *testing.T) {
	manifest, err := testutil.MergeObservabilityManifests(testutil.LinuxServiceObservabilityManifest())
	if err != nil {
		t.Fatalf("validate Linux service observability manifest: %v", err)
	}
	fields := linuxServiceCanaryFields(linuxServiceContentCanary)
	for _, sink := range manifest {
		t.Run(sink.ID, func(t *testing.T) {
			kind := linuxServiceOutputKind(t, sink.Variant)
			got, renderErr := renderSystemdCapture(sink.Boundary, sink.Variant, fields)
			if renderErr != nil {
				t.Fatalf("render systemd capture: %v", renderErr)
			}
			want, renderErr := diagnostics.Render(kind, "service.systemd."+sink.Boundary, fields)
			if renderErr != nil {
				t.Fatalf("render shared diagnostic envelope: %v", renderErr)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("systemd capture bypassed the shared diagnostic renderer:\n got: %s\nwant: %s", got, want)
			}
			text := string(got)
			if strings.Contains(text, linuxServiceContentCanary) {
				t.Fatalf("systemd %s/%s capture leaked content canary: %s", sink.Boundary, sink.Variant, got)
			}
			if strings.Contains(text, "T020_CAUSE_DETAIL_MUST_BE_TRUNCATED") || strings.ContainsAny(text, "\r\t") {
				t.Fatalf("systemd %s/%s capture retained unsafe cause detail: %q", sink.Boundary, sink.Variant, got)
			}
			for _, safe := range []string{"agent-sessions.service", "service.restart", "host", "failed"} {
				if !strings.Contains(text, safe) {
					t.Fatalf("systemd %s/%s capture omitted metadata %q: %s", sink.Boundary, sink.Variant, safe, got)
				}
			}
		})
	}
}

func TestLinuxServiceCaptureRejectsUnmanifestedBoundaryOrVariant(t *testing.T) {
	fields := linuxServiceCanaryFields(linuxServiceContentCanary)
	for _, test := range []struct {
		boundary string
		variant  string
	}{
		{boundary: "syslog", variant: "normal"},
		{boundary: "journal", variant: "verbose-with-content"},
	} {
		if body, err := renderSystemdCapture(test.boundary, test.variant, fields); err == nil {
			t.Fatalf("unmanifested systemd capture %s/%s returned %s", test.boundary, test.variant, body)
		}
	}
}

func linuxServiceOutputKind(t *testing.T, variant string) diagnostics.OutputKind {
	t.Helper()
	switch variant {
	case "normal":
		return diagnostics.OutputNormal
	case "debug":
		return diagnostics.OutputDebug
	case "failure":
		return diagnostics.OutputError
	case "crash":
		return diagnostics.OutputCrashReport
	default:
		t.Fatalf("uncovered Linux service observability variant %q", variant)
		return ""
	}
}

func linuxServiceCanaryFields(canary string) map[string]any {
	return map[string]any{
		"request_id":            "service-request-20",
		"operation":             "service.restart",
		"role":                  "host",
		"identity":              "daemon-generation-8",
		"state":                 "failed",
		"revision":              uint64(20),
		"duration_ms":           int64(31),
		"error_code":            "service_exit",
		"cause_detail":          strings.Repeat("bounded systemd cause detail ", diagnostics.MaxCauseDetailBytes) + "\n\r\tT020_CAUSE_DETAIL_MUST_BE_TRUNCATED",
		"service":               "agent-sessions.service",
		"payload":               canary + "-message",
		"message":               canary + "-peer-message",
		"prompt":                canary + "-prompt",
		"lane_input":            canary + "-lane-input",
		"lane_result":           canary + "-lane-result",
		"tool_arguments":        map[string]any{"query": canary + "-tool-arguments"},
		"tool_result":           canary + "-tool-result",
		"raw_launch_capability": canary + "-capability",
		"credential":            canary + "-credential",
		"vendor_transcript":     canary + "-transcript",
	}
}
