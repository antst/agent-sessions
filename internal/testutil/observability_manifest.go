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
	return coreObservabilityManifest("host", HostCoreObservabilityOwner)
}

type coreObservabilitySink struct {
	suffix   string
	boundary string
	variant  string
	format   string
}

var coreObservabilitySinks = [...]coreObservabilitySink{
	{suffix: "log.normal", boundary: "log", variant: "normal", format: "text"},
	{suffix: "log.debug", boundary: "log", variant: "debug", format: "text"},
	{suffix: "log.error", boundary: "log", variant: "error", format: "text"},
	{suffix: "crash-report", boundary: "crash-report", variant: "failure", format: "json"},
	{suffix: "metric", boundary: "metric", variant: "export", format: "text"},
	{suffix: "trace", boundary: "trace", variant: "export", format: "json"},
	{suffix: "status.human", boundary: "status", variant: "human", format: "text"},
	{suffix: "status.json", boundary: "status", variant: "json", format: "json"},
	{suffix: "doctor.human", boundary: "doctor", variant: "human", format: "text"},
	{suffix: "doctor.json", boundary: "doctor", variant: "json", format: "json"},
}

func coreObservabilityManifest(prefix, owner string) []ObservabilitySink {
	manifest := make([]ObservabilitySink, 0, len(coreObservabilitySinks))
	for _, sink := range coreObservabilitySinks {
		manifest = append(manifest, ObservabilitySink{
			ID:       prefix + "." + sink.suffix,
			Owner:    owner,
			Boundary: sink.boundary,
			Variant:  sink.variant,
			Format:   sink.format,
		})
	}
	return manifest
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
