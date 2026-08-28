package testutil

// HubObservabilityOwner identifies the central-hub observability fragment.
// The platform service-manager fragment is merged separately because the
// shared service engines capture both host and hub roles.
const HubObservabilityOwner = "hub-core"

// HubObservabilityManifest returns the test-owned central-hub boundaries. It
// is acceptance evidence only and is never a production sink registry.
func HubObservabilityManifest() []ObservabilitySink {
	return []ObservabilitySink{
		{ID: "hub.log.normal", Owner: HubObservabilityOwner, Boundary: "log", Variant: "normal", Format: "text"},
		{ID: "hub.log.debug", Owner: HubObservabilityOwner, Boundary: "log", Variant: "debug", Format: "text"},
		{ID: "hub.log.error", Owner: HubObservabilityOwner, Boundary: "log", Variant: "error", Format: "text"},
		{ID: "hub.crash-report", Owner: HubObservabilityOwner, Boundary: "crash-report", Variant: "failure", Format: "json"},
		{ID: "hub.metric", Owner: HubObservabilityOwner, Boundary: "metric", Variant: "export", Format: "text"},
		{ID: "hub.trace", Owner: HubObservabilityOwner, Boundary: "trace", Variant: "export", Format: "json"},
		{ID: "hub.status.human", Owner: HubObservabilityOwner, Boundary: "status", Variant: "human", Format: "text"},
		{ID: "hub.status.json", Owner: HubObservabilityOwner, Boundary: "status", Variant: "json", Format: "json"},
		{ID: "hub.doctor.human", Owner: HubObservabilityOwner, Boundary: "doctor", Variant: "human", Format: "text"},
		{ID: "hub.doctor.json", Owner: HubObservabilityOwner, Boundary: "doctor", Variant: "json", Format: "json"},
	}
}
