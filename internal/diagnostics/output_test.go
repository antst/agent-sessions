package diagnostics

import (
	"strings"
	"testing"
)

const diagnosticContentCanary = "T009_PRIVATE_CONTENT_7f63b52c"

func TestSharedOperationalOutputsExcludeContentCanaries(t *testing.T) {
	fields := diagnosticCanaryFields(diagnosticContentCanary)
	causeDetail := diagnosticOversizedCauseDetail()
	fields["cause_detail"] = causeDetail
	for _, kind := range []OutputKind{
		OutputNormal,
		OutputDebug,
		OutputError,
		OutputCrashReport,
		OutputMetric,
		OutputTrace,
	} {
		t.Run(string(kind), func(t *testing.T) {
			body, err := Render(kind, "operation.transition", fields)
			if err != nil {
				t.Fatalf("render %s output: %v", kind, err)
			}
			assertNoDiagnosticCanary(t, body, diagnosticContentCanary)
			assertRenderedCauseDetailSafe(t, body, causeDetail)
			for _, safe := range []string{"lane.start", "qwen", "accepted", "resource_exhausted"} {
				if !strings.Contains(string(body), safe) {
					t.Fatalf("%s output omitted diagnostic metadata %q: %s", kind, safe, body)
				}
			}
			if kind != OutputMetric && !strings.Contains(string(body), "request-123") {
				t.Fatalf("%s output omitted request correlation metadata: %s", kind, body)
			}
		})
	}
}

func TestSharedCauseDetailIsBoundedAndControlSafe(t *testing.T) {
	detail := diagnosticOversizedCauseDetail()
	bounded := BoundedCauseDetail(detail)
	if len(bounded) > MaxCauseDetailBytes {
		t.Fatalf("bounded cause detail length = %d, limit %d", len(bounded), MaxCauseDetailBytes)
	}
	if strings.ContainsAny(bounded, "\n\r\t") {
		t.Fatalf("bounded cause detail retained control characters: %q", bounded)
	}
	if bounded == "" {
		t.Fatal("bounded cause detail discarded non-secret diagnostic context")
	}
}

func diagnosticCanaryFields(canary string) map[string]any {
	return map[string]any{
		"request_id":   "request-123",
		"operation":    "lane.start",
		"role":         "connector",
		"product":      "qwen",
		"identity":     "session-456",
		"state":        "accepted",
		"revision":     uint64(9),
		"duration_ms":  int64(12),
		"error_code":   "resource_exhausted",
		"cause_detail": "disk allocation failed",
		"payload":      canary + "-message",
		"message":      canary + "-peer-message",
		"prompt":       canary + "-prompt",
		"lane_input":   canary + "-lane-input",
		"lane_result":  canary + "-lane-result",
		"tool_arguments": map[string]any{
			"query": canary + "-tool-argument",
		},
		"tool_result":           canary + "-tool-result",
		"raw_launch_capability": canary + "-raw-launch-capability",
		"credential":            canary + "-credential",
		"vendor_transcript":     canary + "-transcript",
	}
}

func diagnosticOversizedCauseDetail() string {
	return strings.Repeat("bounded non-secret detail ", MaxCauseDetailBytes) +
		"\n\r\tT009_CAUSE_DETAIL_MUST_BE_TRUNCATED"
}

func assertRenderedCauseDetailSafe(t *testing.T, body []byte, unbounded string) {
	t.Helper()
	text := string(body)
	if strings.Contains(text, unbounded) {
		t.Fatal("operational output retained the complete oversized cause detail")
	}
	if strings.Contains(text, "T009_CAUSE_DETAIL_MUST_BE_TRUNCATED") {
		t.Fatalf("operational output retained the cause-detail tail: %s", body)
	}
	if strings.ContainsAny(text, "\r\t") || strings.Contains(text, `\r\t`) {
		t.Fatalf("operational output retained cause-detail control characters: %q", body)
	}
}

func assertNoDiagnosticCanary(t *testing.T, body []byte, canary string) {
	t.Helper()
	if strings.Contains(string(body), canary) {
		t.Fatalf("operational output leaked content canary %q: %s", canary, body)
	}
}

func TestDiagnosticCanaryFixtureNamesEveryForbiddenContentClass(t *testing.T) {
	fields := diagnosticCanaryFields(diagnosticContentCanary)
	for _, key := range []string{
		"payload", "message", "prompt", "lane_input", "lane_result", "tool_arguments",
		"tool_result", "raw_launch_capability", "credential", "vendor_transcript",
	} {
		if _, exists := fields[key]; !exists {
			t.Fatalf("diagnostic content canary fixture omitted %s", key)
		}
	}
}
