package testutil

import (
	"reflect"
	"strings"
	"testing"
)

func TestHostCoreObservabilityManifestIsClosedAndComplete(t *testing.T) {
	manifest, err := MergeObservabilityManifests(HostCoreObservabilityManifest())
	if err != nil {
		t.Fatalf("validate host-core observability manifest: %v", err)
	}

	got := make([]string, 0, len(manifest))
	for _, sink := range manifest {
		if sink.Owner != HostCoreObservabilityOwner {
			t.Fatalf("sink %q owner = %q, want %q", sink.ID, sink.Owner, HostCoreObservabilityOwner)
		}
		got = append(got, sink.ID)
	}
	want := []string{
		"host.crash-report",
		"host.doctor.human",
		"host.doctor.json",
		"host.log.debug",
		"host.log.error",
		"host.log.normal",
		"host.metric",
		"host.status.human",
		"host.status.json",
		"host.trace",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("host-core sink IDs = %q, want %q", got, want)
	}
}

func TestObservabilityManifestFragmentsFailClosed(t *testing.T) {
	base := HostCoreObservabilityManifest()
	duplicate := []ObservabilitySink{{
		ID: "host.log.normal", Owner: "linux-service", Boundary: "journal", Variant: "normal", Format: "text",
	}}
	if _, err := MergeObservabilityManifests(base, duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate merge error = %v, want duplicate diagnostic", err)
	}

	incomplete := []ObservabilitySink{{ID: "host.future"}}
	if _, err := MergeObservabilityManifests(incomplete); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete merge error = %v, want incomplete diagnostic", err)
	}
}

func TestHostCoreObservabilityManifestReturnsIndependentValues(t *testing.T) {
	first := HostCoreObservabilityManifest()
	first[0].ID = "mutated"
	second := HostCoreObservabilityManifest()
	if second[0].ID == "mutated" {
		t.Fatal("host-core manifest returned shared mutable storage")
	}
}
