package daemon

import (
	"context"
	"encoding/json"
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
		body, err := runtime.runtimeStatus(operation)
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
	body, err := runtime.runtimeStatus("status")
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
