package testutil

// HubObservabilityOwner identifies the central-hub observability fragment.
// The platform service-manager fragment is merged separately because the
// shared service engines capture both host and hub roles.
const HubObservabilityOwner = "hub-core"

// HubObservabilityManifest returns the test-owned central-hub boundaries. It
// is acceptance evidence only and is never a production sink registry.
func HubObservabilityManifest() []ObservabilitySink {
	return coreObservabilityManifest("hub", HubObservabilityOwner)
}
