//go:build darwin

package testutil

// DarwinServiceObservabilityOwner identifies launchd-managed output captured
// for the per-user host service. Hub sinks are added by their own fragment.
const DarwinServiceObservabilityOwner = "darwin-service"

// DarwinServiceObservabilityManifest returns the test-owned launchd capture
// boundaries. It is acceptance evidence only, not a production sink registry.
func DarwinServiceObservabilityManifest() []ObservabilitySink {
	return []ObservabilitySink{
		{ID: "host.service.darwin.stderr.crash", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stderr", Variant: "crash", Format: "text"},
		{ID: "host.service.darwin.stderr.debug", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stderr", Variant: "debug", Format: "text"},
		{ID: "host.service.darwin.stderr.failure", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stderr", Variant: "failure", Format: "text"},
		{ID: "host.service.darwin.stderr.normal", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stderr", Variant: "normal", Format: "text"},
		{ID: "host.service.darwin.stdout.crash", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stdout", Variant: "crash", Format: "text"},
		{ID: "host.service.darwin.stdout.debug", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stdout", Variant: "debug", Format: "text"},
		{ID: "host.service.darwin.stdout.failure", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stdout", Variant: "failure", Format: "text"},
		{ID: "host.service.darwin.stdout.normal", Owner: DarwinServiceObservabilityOwner, Boundary: "launchd.stdout", Variant: "normal", Format: "text"},
	}
}
