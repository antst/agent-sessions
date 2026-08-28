package federation

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/testutil"
)

const hubDiagnosticContentCanary = "T066_HUB_PRIVATE_CONTENT_6d3e599a"

func TestHubObservabilityManifestIsClosedCompleteAndIndependent(t *testing.T) {
	manifest, err := testutil.MergeObservabilityManifests(testutil.HubObservabilityManifest())
	if err != nil {
		t.Fatalf("validate hub observability manifest: %v", err)
	}
	want := []string{
		"hub.crash-report", "hub.doctor.human", "hub.doctor.json", "hub.log.debug", "hub.log.error",
		"hub.log.normal", "hub.metric", "hub.status.human", "hub.status.json", "hub.trace",
	}
	got := make([]string, 0, len(manifest))
	for _, sink := range manifest {
		got = append(got, sink.ID)
		if sink.Owner != testutil.HubObservabilityOwner {
			t.Errorf("hub sink %q owner = %q", sink.ID, sink.Owner)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hub observability sinks = %q, want exact closed inventory %q", got, want)
	}
	first := testutil.HubObservabilityManifest()
	first[0].ID = "mutated"
	if second := testutil.HubObservabilityManifest(); second[0].ID == "mutated" {
		t.Fatal("hub observability manifest returned shared mutable storage")
	}
}

func TestEveryHubCoreObservabilitySinkUsesMetadataOnlyContentCanary(t *testing.T) {
	manifest, err := testutil.MergeObservabilityManifests(testutil.HubObservabilityManifest())
	if err != nil {
		t.Fatal(err)
	}
	renderers := hubCanaryRenderers()
	if len(renderers) != len(manifest) {
		t.Fatalf("hub renderers = %d, manifest sinks = %d", len(renderers), len(manifest))
	}
	fields := hubDiagnosticCanaryFields()
	fields["cause_detail"] = hubOversizedCauseDetail()
	for _, sink := range manifest {
		render, exists := renderers[sink.ID]
		if !exists {
			t.Fatalf("hub sink %q bypasses the content canary", sink.ID)
		}
		t.Run(sink.ID, func(t *testing.T) {
			body, renderErr := render(fields)
			if renderErr != nil {
				t.Fatalf("render hub sink: %v", renderErr)
			}
			text := string(body)
			if strings.Contains(text, hubDiagnosticContentCanary) {
				t.Fatalf("hub sink leaked content or host lifecycle authority: %s", body)
			}
			for _, safe := range []string{"hub", "127.0.0.1:7443", "3"} {
				if !strings.Contains(text, safe) {
					t.Fatalf("hub sink omitted diagnostic metadata %q: %s", safe, body)
				}
			}
			assertHubCauseDetailSafe(t, body)
		})
	}
}

func TestHubServiceManagerCapturedOutputUsesTheSameContentPolicy(t *testing.T) {
	fields := hubDiagnosticCanaryFields()
	fields["cause_detail"] = hubOversizedCauseDetail()
	kinds := map[string]diagnostics.OutputKind{
		"normal": diagnostics.OutputNormal, "debug": diagnostics.OutputDebug,
		"failure": diagnostics.OutputError, "crash": diagnostics.OutputCrashReport,
	}
	for _, boundary := range []string{"journal", "stdout", "stderr"} {
		for variant, kind := range kinds {
			t.Run(boundary+"/"+variant, func(t *testing.T) {
				body, err := renderHubDiagnostic(kind, "hub.service-manager."+boundary, fields)
				if err != nil {
					t.Fatal(err)
				}
				text := string(body)
				if strings.Contains(text, hubDiagnosticContentCanary) {
					t.Fatalf("hub service-manager capture leaked private content: %s", body)
				}
				for _, metadata := range []string{"hub", "agent-sessions-hub", "127.0.0.1:7443"} {
					if !strings.Contains(text, metadata) {
						t.Fatalf("hub service-manager capture omitted %q: %s", metadata, body)
					}
				}
				assertHubCauseDetailSafe(t, body)
			})
		}
	}
}

func TestHubStatusAndDoctorHaveStableSharedEnvelopeWithoutHostAuthority(t *testing.T) {
	fields := hubDiagnosticCanaryFields()
	tests := []struct {
		name   string
		event  string
		render func(map[string]any, bool) ([]byte, error)
	}{
		{name: "status", event: "hub.status", render: renderHubStatus},
		{name: "doctor", event: "hub.doctor", render: renderHubDoctor},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine, err := test.render(fields, true)
			if err != nil {
				t.Fatal(err)
			}
			var envelope diagnostics.Envelope
			if err := json.Unmarshal(machine, &envelope); err != nil {
				t.Fatalf("decode stable hub envelope: %v: %s", err, machine)
			}
			if envelope.SchemaVersion != diagnostics.EnvelopeSchemaVersion || envelope.Event != test.event {
				t.Fatalf("hub envelope = %+v", envelope)
			}
			for _, key := range []string{"runtime_version", "runtime_identity", "pid", "proc_start", "service", "protocol_version", "listener"} {
				if _, exists := envelope.Metadata[key]; !exists {
					t.Errorf("hub %s omitted stable metadata field %q: %s", test.name, key, machine)
				}
			}
			for _, forbidden := range []string{"host_id", "host_name", "products", "attachments", "lanes", "federation", "endpoint"} {
				if _, exists := envelope.Metadata[forbidden]; exists {
					t.Errorf("hub %s claimed host authority field %q: %s", test.name, forbidden, machine)
				}
			}
			if strings.Contains(string(machine), hubDiagnosticContentCanary) {
				t.Fatalf("hub %s leaked private canary: %s", test.name, machine)
			}
			human, err := test.render(fields, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(human), test.event) || strings.Contains(string(human), hubDiagnosticContentCanary) {
				t.Fatalf("hub human %s is unstable or unsafe: %s", test.name, human)
			}
		})
	}
}

func TestHubDoctorRetainsCauseSpecificBoundedRemediation(t *testing.T) {
	for _, failure := range []struct {
		code   string
		action string
	}{
		{code: "disk_full", action: "free space in the hub state filesystem and retry"},
		{code: "memory_unavailable", action: "restore memory availability and retry"},
		{code: "file_descriptors_exhausted", action: "restore the owning user's file descriptor availability and retry"},
		{code: "process_resources_exhausted", action: "restore process resources and retry"},
		{code: "hub_listener_unavailable", action: "verify the configured listener address and service state"},
	} {
		t.Run(failure.code, func(t *testing.T) {
			fields := hubDiagnosticCanaryFields()
			fields["error_code"] = failure.code
			fields["next_action"] = failure.action
			fields["cause_detail"] = hubOversizedCauseDetail()
			body, err := renderHubDoctor(fields, true)
			if err != nil {
				t.Fatal(err)
			}
			text := string(body)
			if !strings.Contains(text, failure.code) || !strings.Contains(text, failure.action) {
				t.Fatalf("hub doctor lost cause-specific remediation: %s", body)
			}
			assertHubCauseDetailSafe(t, body)
		})
	}
}

func hubCanaryRenderers() map[string]func(map[string]any) ([]byte, error) {
	return map[string]func(map[string]any) ([]byte, error){
		"hub.log.normal": func(fields map[string]any) ([]byte, error) {
			return renderHubDiagnostic(diagnostics.OutputNormal, "hub.operation", fields)
		},
		"hub.log.debug": func(fields map[string]any) ([]byte, error) {
			return renderHubDiagnostic(diagnostics.OutputDebug, "hub.operation", fields)
		},
		"hub.log.error": func(fields map[string]any) ([]byte, error) {
			return renderHubDiagnostic(diagnostics.OutputError, "hub.operation", fields)
		},
		"hub.crash-report": func(fields map[string]any) ([]byte, error) {
			return renderHubDiagnostic(diagnostics.OutputCrashReport, "hub.operation", fields)
		},
		"hub.metric": func(fields map[string]any) ([]byte, error) {
			return renderHubDiagnostic(diagnostics.OutputMetric, "hub.operation", fields)
		},
		"hub.trace": func(fields map[string]any) ([]byte, error) {
			return renderHubDiagnostic(diagnostics.OutputTrace, "hub.operation", fields)
		},
		"hub.status.human": func(fields map[string]any) ([]byte, error) { return renderHubStatus(fields, false) },
		"hub.status.json":  func(fields map[string]any) ([]byte, error) { return renderHubStatus(fields, true) },
		"hub.doctor.human": func(fields map[string]any) ([]byte, error) { return renderHubDoctor(fields, false) },
		"hub.doctor.json":  func(fields map[string]any) ([]byte, error) { return renderHubDoctor(fields, true) },
	}
}

func hubDiagnosticCanaryFields() map[string]any {
	canary := hubDiagnosticContentCanary
	return map[string]any{
		"request_id":       "hub-request-1",
		"operation":        "hub.route",
		"role":             "hub",
		"runtime_version":  "hub-release-9",
		"runtime_identity": "sha256:" + strings.Repeat("9", 64),
		"pid":              909,
		"proc_start":       "909:12345",
		"service":          map[string]any{"manager": "user", "unit": "agent-sessions-hub"},
		"state":            "ready",
		"revision":         uint64(12),
		"protocol_version": ProtocolVersion,
		"listener":         "127.0.0.1:7443",
		"connected_hosts":  []string{"host-a", "host-b"},
		"routing":          map[string]any{"healthy": true, "pending": 0},
		"debt":             []any{},
		"healthy":          true,
		"checks":           []any{map[string]any{"id": "listener", "healthy": true}},
		"error_code":       "none",
		"retryable":        false,
		"next_action":      "none",

		"host_id":     canary + "-remote-host-lifecycle",
		"host_name":   canary + "-remote-host-name",
		"products":    map[string]any{"codex": canary + "-product-state"},
		"attachments": canary + "-attachment-state",
		"lanes":       canary + "-lane-state",
		"federation":  canary + "-host-federation-state",
		"endpoint":    canary + "-host-endpoint",

		"payload":               canary + "-message",
		"prompt":                canary + "-prompt",
		"lane_result":           canary + "-lane-result",
		"tool_arguments":        canary + "-tool-arguments",
		"tool_result":           canary + "-tool-result",
		"credential":            canary + "-credential",
		"token":                 canary + "-token",
		"vendor_transcript":     canary + "-transcript",
		"raw_launch_capability": canary + "-capability",
	}
}

func hubOversizedCauseDetail() string {
	return strings.Repeat("bounded hub cause detail ", diagnostics.MaxCauseDetailBytes) +
		"\n\r\tT066_HUB_CAUSE_DETAIL_MUST_BE_TRUNCATED"
}

func assertHubCauseDetailSafe(t *testing.T, body []byte) {
	t.Helper()
	text := string(body)
	if strings.Contains(text, "T066_HUB_CAUSE_DETAIL_MUST_BE_TRUNCATED") ||
		strings.ContainsAny(text, "\r\t") || strings.Contains(text, `\r\t`) {
		t.Fatalf("hub sink retained unbounded/control-character cause detail: %s", body)
	}
}
