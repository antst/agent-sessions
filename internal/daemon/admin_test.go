package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

func TestAdminReportsTruthfulCountsWithoutCatalogContent(t *testing.T) {
	const canary = "SECRET_CREDENTIAL_PROMPT_RESULT_TRANSCRIPT"
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{StateRoot: shortDaemonTestRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Host.User = canary
	catalog.Host.Host = canary
	catalog.Host.Release = canary
	catalog.Host.Endpoint = canary
	catalog.Host.ProductReadiness = map[string]string{"codex": "ready", "claude": canary}
	catalog.Attachments["attachment"] = ManagedAttachment{ID: "attachment", Product: "codex", Name: canary, Cwd: canary, State: "attached", DaemonGeneration: runtime.Generation()}
	catalog.Deliveries["delivery"] = Delivery{ID: "delivery", Sender: canary, Destinations: []string{canary}, Acknowledgment: canary, State: "accepted"}
	catalog.Lanes["lane"] = Lane{ID: "lane", ParentAttachmentID: "attachment", Product: "codex", Name: canary, State: "terminal"}
	catalog.Turns["turn"] = Turn{ID: "turn", LaneID: "lane", Sequence: 1, State: "terminal", Result: canary, Diagnostic: canary, TerminalRevision: 1}
	catalog.CleanupDebts["debt"] = CleanupDebt{ID: "debt", Resource: "lane", BaselineIdentity: canary, IntendedState: "absent", LastVerifiedState: canary, Cause: canary, Operation: "archive"}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"status", "doctor"} {
		body, err := runtime.runtimeStatus(context.Background(), operation)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), canary) {
			t.Fatalf("%s leaked catalog content: %s", operation, body)
		}
		if len(body) > diagnostics.MaxReportBytes {
			t.Fatalf("%s output is unbounded: %d", operation, len(body))
		}
		var report diagnostics.Report
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		if report.Schema != diagnostics.Schema || report.Operation != operation || !report.Ready {
			t.Fatalf("%s report = %+v", operation, report)
		}
		if report.Records.Attachments != 1 || report.Records.ActiveAttachments != 1 || report.Records.UncollectedTurns != 1 || report.Records.CleanupDebts != 1 {
			t.Fatalf("%s counts = %+v", operation, report.Records)
		}
		for _, product := range report.Products {
			if product.Readiness != "unknown" {
				t.Fatalf("%s consumed durable product readiness: %+v", operation, report.Products)
			}
		}
		if operation == "doctor" && len(report.Checks) == 0 {
			t.Fatal("doctor omitted checks")
		}
	}
}

func TestAdminReportsConfiguredReleasePresenceWithoutPublishingReleaseBytes(t *testing.T) {
	const releaseCanary = "release-SECRET-CANARY"
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{StateRoot: shortDaemonTestRoot(t), Release: releaseCanary})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	body, err := runtime.runtimeStatus(context.Background(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), releaseCanary) || !strings.Contains(string(body), `"release_present":true`) {
		t.Fatalf("release projection = %s", body)
	}
	snapshot, err := runtime.State().Read()
	if err != nil || snapshot.Catalog.Host.Release != releaseCanary {
		t.Fatalf("durable release = %q, %v", snapshot.Catalog.Host.Release, err)
	}
}

func TestAdminConsumesOnlyLiveBoundedProductDiagnostics(t *testing.T) {
	const canary = "LIVE_PROVIDER_SECRET_DIAGNOSTIC"
	var calls []string
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: shortDaemonTestRoot(t),
		ProductDiagnosticsProvider: func(_ context.Context, operation string) (map[string]string, error) {
			calls = append(calls, operation)
			states := map[string]string{"codex": "ready", "claude": "missing", "grok": canary}
			for index := 0; index < 10000; index++ {
				states[strings.Repeat("x", 64)+string(rune(index))] = strings.Repeat(canary, 64)
			}
			return states, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	snapshot, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog
	catalog.Host.ProductReadiness = map[string]string{"codex": "missing", "claude": canary}
	if _, err := runtime.State().Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"status", "doctor"} {
		body, err := runtime.runtimeStatus(context.Background(), operation)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > diagnostics.MaxReportBytes || strings.Contains(string(body), canary) {
			t.Fatalf("%s live diagnostic output is unsafe: %d bytes: %s", operation, len(body), body)
		}
		var report diagnostics.Report
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		readiness := map[string]string{}
		for _, product := range report.Products {
			readiness[product.ID] = product.Readiness
		}
		if readiness["codex"] != "available" || readiness["claude"] != "unavailable" || readiness["grok"] != "unknown" {
			t.Fatalf("%s live product readiness = %+v", operation, readiness)
		}
	}
	if len(calls) != 2 || calls[0] != "status" || calls[1] != "doctor" {
		t.Fatalf("live diagnostics calls = %+v", calls)
	}
}

func TestAdminFailsClosedWhenLiveDiagnosticsFails(t *testing.T) {
	const canary = "VENDOR_SECRET_DIAGNOSTIC"
	runtime, err := StartRuntime(context.Background(), RuntimeConfig{
		StateRoot: shortDaemonTestRoot(t),
		ProductDiagnosticsProvider: func(context.Context, string) (map[string]string, error) {
			return nil, errors.New(canary)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	body, err := runtime.runtimeStatus(context.Background(), "doctor")
	if err == nil || strings.Contains(err.Error(), canary) || body != nil {
		t.Fatalf("direct diagnostic failure = %q, %v", body, err)
	}
	response, err := CallControl(context.Background(), runtime.Endpoint(), ControlRequest{
		ID: "doctor", Role: RoleAdmin, Operation: "doctor", Generation: runtime.Generation(),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(response)
	if response.OK || response.Error == nil || strings.Contains(string(encoded), canary) {
		t.Fatalf("control diagnostic failure = %s", encoded)
	}
}
