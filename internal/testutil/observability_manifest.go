package testutil

import (
	"fmt"
	"sort"
)

// ObservabilitySink describes one operational output boundary exercised by
// acceptance tests. It is test evidence only; production code must not import
// this package or use the manifest as a runtime sink registry.
type ObservabilitySink struct {
	ID       string
	Owner    string
	Boundary string
	Variant  string
	Format   string
}

// HostCoreObservabilityOwner identifies the foundational host sink fragment.
const HostCoreObservabilityOwner = "host-core"

// HostCoreObservabilityManifest returns the foundational host sinks. Later
// service-manager and hub tasks add file-disjoint fragments and combine them
// with MergeObservabilityManifests in tests.
func HostCoreObservabilityManifest() []ObservabilitySink {
	return []ObservabilitySink{
		{ID: "host.log.normal", Owner: HostCoreObservabilityOwner, Boundary: "log", Variant: "normal", Format: "text"},
		{ID: "host.log.debug", Owner: HostCoreObservabilityOwner, Boundary: "log", Variant: "debug", Format: "text"},
		{ID: "host.log.error", Owner: HostCoreObservabilityOwner, Boundary: "log", Variant: "error", Format: "text"},
		{ID: "host.crash-report", Owner: HostCoreObservabilityOwner, Boundary: "crash-report", Variant: "failure", Format: "json"},
		{ID: "host.metric", Owner: HostCoreObservabilityOwner, Boundary: "metric", Variant: "export", Format: "text"},
		{ID: "host.trace", Owner: HostCoreObservabilityOwner, Boundary: "trace", Variant: "export", Format: "json"},
		{ID: "host.status.human", Owner: HostCoreObservabilityOwner, Boundary: "status", Variant: "human", Format: "text"},
		{ID: "host.status.json", Owner: HostCoreObservabilityOwner, Boundary: "status", Variant: "json", Format: "json"},
		{ID: "host.doctor.human", Owner: HostCoreObservabilityOwner, Boundary: "doctor", Variant: "human", Format: "text"},
		{ID: "host.doctor.json", Owner: HostCoreObservabilityOwner, Boundary: "doctor", Variant: "json", Format: "json"},
	}
}

// MergeObservabilityManifests validates and combines test-owned fragments in
// deterministic ID order. Duplicate sink IDs fail closed so a new boundary
// cannot silently replace an existing canary.
func MergeObservabilityManifests(fragments ...[]ObservabilitySink) ([]ObservabilitySink, error) {
	merged := make([]ObservabilitySink, 0)
	seen := make(map[string]struct{})
	for _, fragment := range fragments {
		for _, sink := range fragment {
			if sink.ID == "" || sink.Owner == "" || sink.Boundary == "" || sink.Variant == "" || sink.Format == "" {
				return nil, fmt.Errorf("observability sink has incomplete identity: %+v", sink)
			}
			if _, exists := seen[sink.ID]; exists {
				return nil, fmt.Errorf("duplicate observability sink %q", sink.ID)
			}
			seen[sink.ID] = struct{}{}
			merged = append(merged, sink)
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged, nil
}
