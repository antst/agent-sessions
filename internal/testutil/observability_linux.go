//go:build linux

package testutil

// LinuxServiceObservabilityOwner identifies the systemd user-service fragment.
const LinuxServiceObservabilityOwner = "linux-service"

// LinuxServiceObservabilityManifest returns the test-owned systemd capture
// boundaries. Each service output class is checked at the journal and at the
// daemon stdout/stderr streams captured by the user manager.
func LinuxServiceObservabilityManifest() []ObservabilitySink {
	return []ObservabilitySink{
		{ID: "linux.systemd.journal.normal", Owner: LinuxServiceObservabilityOwner, Boundary: "journal", Variant: "normal", Format: "json"},
		{ID: "linux.systemd.journal.debug", Owner: LinuxServiceObservabilityOwner, Boundary: "journal", Variant: "debug", Format: "json"},
		{ID: "linux.systemd.journal.failure", Owner: LinuxServiceObservabilityOwner, Boundary: "journal", Variant: "failure", Format: "json"},
		{ID: "linux.systemd.journal.crash", Owner: LinuxServiceObservabilityOwner, Boundary: "journal", Variant: "crash", Format: "json"},
		{ID: "linux.systemd.stdout.normal", Owner: LinuxServiceObservabilityOwner, Boundary: "stdout", Variant: "normal", Format: "json"},
		{ID: "linux.systemd.stdout.debug", Owner: LinuxServiceObservabilityOwner, Boundary: "stdout", Variant: "debug", Format: "json"},
		{ID: "linux.systemd.stdout.failure", Owner: LinuxServiceObservabilityOwner, Boundary: "stdout", Variant: "failure", Format: "json"},
		{ID: "linux.systemd.stdout.crash", Owner: LinuxServiceObservabilityOwner, Boundary: "stdout", Variant: "crash", Format: "json"},
		{ID: "linux.systemd.stderr.normal", Owner: LinuxServiceObservabilityOwner, Boundary: "stderr", Variant: "normal", Format: "json"},
		{ID: "linux.systemd.stderr.debug", Owner: LinuxServiceObservabilityOwner, Boundary: "stderr", Variant: "debug", Format: "json"},
		{ID: "linux.systemd.stderr.failure", Owner: LinuxServiceObservabilityOwner, Boundary: "stderr", Variant: "failure", Format: "json"},
		{ID: "linux.systemd.stderr.crash", Owner: LinuxServiceObservabilityOwner, Boundary: "stderr", Variant: "crash", Format: "json"},
	}
}

// PlatformServiceObservabilityManifest returns the service-manager fragment
// applicable to this test binary's platform.
func PlatformServiceObservabilityManifest() []ObservabilitySink {
	return append([]ObservabilitySink(nil), LinuxServiceObservabilityManifest()...)
}
