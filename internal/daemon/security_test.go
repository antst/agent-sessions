package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/testutil"
)

func TestAggregateObservabilityManifestCoversApplicablePlatformAndHub(t *testing.T) {
	host := testutil.HostCoreObservabilityManifest()
	service := testutil.PlatformServiceObservabilityManifest()
	hub := testutil.HubObservabilityManifest()

	merged, err := testutil.MergeObservabilityManifests(host, service, hub)
	if err != nil {
		t.Fatalf("merge applicable observability manifest: %v", err)
	}
	if got, want := len(merged), len(host)+len(service)+len(hub); got != want {
		t.Fatalf("applicable observability sinks = %d, want host + platform service + hub = %d", got, want)
	}

	wantByID := make(map[string]testutil.ObservabilitySink, len(merged))
	for _, fragment := range [][]testutil.ObservabilitySink{host, service, hub} {
		for _, sink := range fragment {
			wantByID[sink.ID] = sink
		}
	}
	for index, sink := range merged {
		if index > 0 && merged[index-1].ID >= sink.ID {
			t.Fatalf("merged observability manifest is not strictly ordered: %q before %q", merged[index-1].ID, sink.ID)
		}
		if want, exists := wantByID[sink.ID]; !exists || !reflect.DeepEqual(sink, want) {
			t.Fatalf("merged sink %q is not the unchanged authoritative fragment entry: got %+v want %+v", sink.ID, sink, want)
		}
		delete(wantByID, sink.ID)
	}
	if len(wantByID) != 0 {
		missing := make([]string, 0, len(wantByID))
		for id := range wantByID {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		t.Fatalf("applicable observability union omitted fragment sinks: %q", missing)
	}

	requireCoreObservabilityCoverage(t, testutil.HostCoreObservabilityOwner, host)
	requireServiceObservabilityCoverage(t, runtime.GOOS+"-service", service)
	requireCoreObservabilityCoverage(t, testutil.HubObservabilityOwner, hub)

	first := testutil.PlatformServiceObservabilityManifest()
	if len(first) == 0 {
		t.Fatal("applicable platform service observability fragment is empty")
	}
	first[0].ID = "mutated"
	if second := testutil.PlatformServiceObservabilityManifest(); len(second) == 0 || second[0].ID == "mutated" {
		t.Fatal("applicable platform observability delegate returned shared mutable storage")
	}
}

func TestAggregateObservabilityContentAndSecretCanaries(t *testing.T) {
	root := testutil.ShortSocketRoot(t, "a90-", filepath.Join("run", "daemon.sock"))
	for _, gate := range aggregateSecurityGates(t) {
		t.Run(gate.name, func(t *testing.T) {
			runExistingGoTestGate(t, root, gate)
		})
	}
}

type aggregateGoTestGate struct {
	name        string
	packagePath string
	tests       []string
}

func aggregateSecurityGates(t *testing.T) []aggregateGoTestGate {
	t.Helper()
	var serviceTests []string
	switch runtime.GOOS {
	case "linux":
		serviceTests = []string{
			"TestLinuxServiceObservabilityManifestIsClosedAndComplete",
			"TestEveryLinuxServiceCaptureDelegatesToSharedContentCanary",
		}
	case "darwin":
		serviceTests = []string{
			"TestDarwinServiceObservabilityManifestIsClosed",
			"TestDarwinLaunchdCapturedOutputExcludesContentCanaries",
		}
	default:
		t.Fatalf("aggregate observability gate has no service fragment for %s", runtime.GOOS)
	}
	return []aggregateGoTestGate{
		{
			name: "shared diagnostic content classes", packagePath: "./internal/diagnostics",
			tests: []string{
				"TestSharedOperationalOutputsExcludeContentCanaries",
				"TestDiagnosticCanaryFixtureNamesEveryForbiddenContentClass",
			},
		},
		{
			name: "host peer and lane boundaries", packagePath: "./internal/daemon",
			tests: []string{
				"TestEveryHostCoreObservabilitySinkUsesContentCanary",
				"TestDeliveryDiagnosticsNeverContainMessageContent",
				"TestLaneTerminalNoticeAndConcurrentCollectionAreExactlyOnce",
				"TestLaneCommandMapsCanonicalLifecycleToDaemonLaneEngine",
			},
		},
		{name: runtime.GOOS + " service-manager capture", packagePath: "./internal/servicecontrol", tests: serviceTests},
		{
			name: "hub and remote lane boundaries", packagePath: "./internal/federation",
			tests: []string{
				"TestHubObservabilityManifestIsClosedCompleteAndIndependent",
				"TestEveryHubCoreObservabilitySinkUsesMetadataOnlyContentCanary",
				"TestHubServiceManagerCapturedOutputUsesTheSameContentPolicy",
				"TestHubStatusAndDoctorHaveStableSharedEnvelopeWithoutHostAuthority",
				"TestHubDoctorRetainsCauseSpecificBoundedRemediation",
				"TestRemoteLaneResultPublishesOneContentFreeParentNotice",
			},
		},
	}
}

func requireCoreObservabilityCoverage(t *testing.T, owner string, sinks []testutil.ObservabilitySink) {
	t.Helper()
	want := map[string][]string{
		"log":          {"normal", "debug", "error"},
		"crash-report": {"failure"},
		"metric":       {"export"},
		"trace":        {"export"},
		"status":       {"human", "json"},
		"doctor":       {"human", "json"},
	}
	covered := observabilityCoverage(t, owner, sinks)
	for boundary, variants := range want {
		for _, variant := range variants {
			if !covered[boundary+"/"+variant] {
				t.Errorf("%s observability fragment omits %s/%s", owner, boundary, variant)
			}
		}
	}
}

func requireServiceObservabilityCoverage(t *testing.T, owner string, sinks []testutil.ObservabilitySink) {
	t.Helper()
	covered := observabilityCoverage(t, owner, sinks)
	boundaries := make(map[string]bool)
	for _, sink := range sinks {
		boundaries[sink.Boundary] = true
	}
	if len(boundaries) < 2 {
		t.Fatalf("%s observability fragment has %d capture boundaries, want stdout and stderr coverage", owner, len(boundaries))
	}
	for boundary := range boundaries {
		for _, variant := range []string{"normal", "debug", "failure", "crash"} {
			if !covered[boundary+"/"+variant] {
				t.Errorf("%s observability fragment omits %s/%s", owner, boundary, variant)
			}
		}
	}
}

func observabilityCoverage(t *testing.T, owner string, sinks []testutil.ObservabilitySink) map[string]bool {
	t.Helper()
	covered := make(map[string]bool, len(sinks))
	for _, sink := range sinks {
		if sink.Owner != owner {
			t.Errorf("observability sink %q owner = %q, want %q", sink.ID, sink.Owner, owner)
		}
		covered[sink.Boundary+"/"+sink.Variant] = true
	}
	return covered
}

func runExistingGoTestGate(t *testing.T, socketRoot string, gate aggregateGoTestGate) {
	t.Helper()
	if len(gate.tests) == 0 {
		t.Fatal("aggregate gate names no existing tests")
	}
	patterns := make([]string, 0, len(gate.tests))
	for _, name := range gate.tests {
		patterns = append(patterns, regexp.QuoteMeta(name))
	}
	pattern := "^(" + strings.Join(patterns, "|") + ")$"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", gate.packagePath, "-run", pattern, "-count=1")
	command.Dir = aggregateRepositoryRoot(t)
	command.Env = replaceAggregateEnvironment(os.Environ(), "TMPDIR", socketRoot)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("rerun existing %s tests timed out: %v", gate.packagePath, ctx.Err())
	}
	if err != nil {
		t.Fatalf("rerun existing %s tests %v: %v\n%s", gate.packagePath, gate.tests, err, output)
	}
}

func aggregateRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve aggregate harness source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func replaceAggregateEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	replaced := false
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				result = append(result, prefix+value)
				replaced = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, fmt.Sprintf("%s=%s", key, value))
	}
	return result
}
