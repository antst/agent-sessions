package daemon

import (
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/testutil"
)

const hostDiagnosticContentCanary = "T009_HOST_PRIVATE_CONTENT_b8f2110a"

func TestEveryHostCoreObservabilitySinkUsesContentCanary(t *testing.T) {
	renderers := hostCoreCanaryRenderers()
	manifest, err := testutil.MergeObservabilityManifests(testutil.HostCoreObservabilityManifest())
	if err != nil {
		t.Fatalf("validate host observability manifest: %v", err)
	}
	if len(renderers) != len(manifest) {
		t.Fatalf("host canary renderers = %d, manifest sinks = %d", len(renderers), len(manifest))
	}

	fields := hostDiagnosticCanaryFields(hostDiagnosticContentCanary)
	causeDetail := hostOversizedCauseDetail()
	fields["cause_detail"] = causeDetail
	for _, sink := range manifest {
		render, exists := renderers[sink.ID]
		if !exists {
			t.Fatalf("host-core sink %q bypasses the content canary", sink.ID)
		}
		t.Run(sink.ID, func(t *testing.T) {
			body, renderErr := render(fields)
			if renderErr != nil {
				t.Fatalf("render host-core sink: %v", renderErr)
			}
			if strings.Contains(string(body), hostDiagnosticContentCanary) {
				t.Fatalf("host-core sink leaked content canary: %s", body)
			}
			assertHostRenderedCauseDetailSafe(t, body, causeDetail)
			for _, safe := range hostCoreExpectedMetadata(sink.ID) {
				if !strings.Contains(string(body), safe) {
					t.Fatalf("host-core sink omitted diagnostic metadata %q: %s", safe, body)
				}
			}
		})
	}
}

func hostCoreExpectedMetadata(sinkID string) []string {
	switch sinkID {
	case "host.log.normal", "host.log.debug", "host.log.error", "host.trace":
		return []string{"request-host-1", "delivery.accept", "codex", "ready"}
	case "host.crash-report":
		return []string{"delivery.accept", "codex", "none"}
	case "host.metric":
		return []string{"delivery.accept", "codex", "ready"}
	case "host.status.human", "host.status.json":
		return []string{"codex", "ready", "4"}
	case "host.doctor.human", "host.doctor.json":
		return []string{"codex", "ready", "none"}
	default:
		return nil
	}
}

func hostCoreCanaryRenderers() map[string]func(map[string]any) ([]byte, error) {
	return map[string]func(map[string]any) ([]byte, error){
		"host.log.normal": func(fields map[string]any) ([]byte, error) {
			return renderHostLog(diagnostics.OutputNormal, "host.operation", fields)
		},
		"host.log.debug": func(fields map[string]any) ([]byte, error) {
			return renderHostLog(diagnostics.OutputDebug, "host.operation", fields)
		},
		"host.log.error": func(fields map[string]any) ([]byte, error) {
			return renderHostLog(diagnostics.OutputError, "host.operation", fields)
		},
		"host.crash-report": func(fields map[string]any) ([]byte, error) {
			return diagnostics.Render(diagnostics.OutputCrashReport, "host.operation", fields)
		},
		"host.metric": func(fields map[string]any) ([]byte, error) {
			return diagnostics.Render(diagnostics.OutputMetric, "host.operation", fields)
		},
		"host.trace": func(fields map[string]any) ([]byte, error) {
			return diagnostics.Render(diagnostics.OutputTrace, "host.operation", fields)
		},
		"host.status.human": func(fields map[string]any) ([]byte, error) {
			return renderHostStatus(fields, false)
		},
		"host.status.json": func(fields map[string]any) ([]byte, error) {
			return renderHostStatus(fields, true)
		},
		"host.doctor.human": func(fields map[string]any) ([]byte, error) {
			return renderHostDoctor(fields, false)
		},
		"host.doctor.json": func(fields map[string]any) ([]byte, error) {
			return renderHostDoctor(fields, true)
		},
	}
}

func hostDiagnosticCanaryFields(canary string) map[string]any {
	return map[string]any{
		"request_id":            "request-host-1",
		"operation":             "delivery.accept",
		"role":                  "daemon",
		"product":               "codex",
		"identity":              "attachment-1",
		"state":                 "ready",
		"revision":              uint64(4),
		"duration_ms":           int64(8),
		"error_code":            "none",
		"payload":               canary + "-message",
		"prompt":                canary + "-prompt",
		"lane_result":           canary + "-lane-result",
		"tool_arguments":        canary + "-tool-arguments",
		"tool_result":           canary + "-tool-result",
		"raw_launch_capability": canary + "-raw-launch-capability",
		"credential":            canary + "-credential",
		"vendor_transcript":     canary + "-transcript",
	}
}

func hostOversizedCauseDetail() string {
	return strings.Repeat("bounded host cause detail ", diagnostics.MaxCauseDetailBytes) +
		"\n\r\tT009_HOST_CAUSE_DETAIL_MUST_BE_TRUNCATED"
}

func assertHostRenderedCauseDetailSafe(t *testing.T, body []byte, unbounded string) {
	t.Helper()
	text := string(body)
	if strings.Contains(text, unbounded) {
		t.Fatal("host-core sink retained the complete oversized cause detail")
	}
	if strings.Contains(text, "T009_HOST_CAUSE_DETAIL_MUST_BE_TRUNCATED") {
		t.Fatalf("host-core sink retained the cause-detail tail: %s", body)
	}
	if strings.ContainsAny(text, "\r\t") || strings.Contains(text, `\r\t`) {
		t.Fatalf("host-core sink retained cause-detail control characters: %q", body)
	}
}
