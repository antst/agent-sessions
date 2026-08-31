package acceptance

import (
	"strings"
	"testing"
)

func TestMatrixRunnerRejectsMissingDuplicateAndUnknownCellIDs(t *testing.T) {
	manifest, cells := loadResultContractManifest(t)
	valid := validAcceptanceResult("source-pass", "S-01", "linux")
	tests := []struct {
		name string
		run  Run
		want string
	}{
		{name: "missing", run: Run{RequestedCellIDs: []string{"S-01"}}, want: "missing result"},
		{name: "duplicate requested", run: Run{RequestedCellIDs: []string{"S-01", "S-01"}, Results: []Result{valid}}, want: "duplicate requested"},
		{name: "unknown", run: Run{RequestedCellIDs: []string{"UNKNOWN"}, Results: []Result{valid}}, want: "unknown cell"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateResults(manifest, cells, test.run); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateResults() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestMatrixRunnerRequiresRealDestinationVisibleEvidence(t *testing.T) {
	manifest, cells := loadResultContractManifest(t)
	base := []Result{
		validAcceptanceResult("source-pass", "S-01", "linux"),
		validAcceptanceResult("install-u01", "U-01", "linux"),
		validAcceptanceResult("install-u08", "U-08", "linux"),
	}
	for index := range base {
		base[index].NativeProductVersions["grok"] = "real-version"
	}

	fake := validAcceptanceResult("message-fake", "M-CP-GP", "linux")
	fake.NativeProductVersions["grok"] = "real-version"
	fake.ExactIdentityEvidence = ExactIdentityEvidence{Kind: "fake-vendor", Product: "grok"}
	if err := ValidateResults(manifest, cells, Run{
		RequestedCellIDs: []string{"M-CP-GP"}, Results: append(append([]Result{}, base...), fake),
	}); err == nil || !strings.Contains(err.Error(), "real installed") {
		t.Fatalf("fake messaging evidence error = %v, want real installed rejection", err)
	}

	for _, cellID := range []string{"M-CP-GP", "P-C-G"} {
		missingDestination := validAcceptanceResult("no-destination-"+cellID, cellID, "linux")
		missingDestination.NativeProductVersions["grok"] = "real-version"
		if err := ValidateResults(manifest, cells, Run{
			RequestedCellIDs: []string{cellID}, Results: append(append([]Result{}, base...), missingDestination),
		}); err == nil || !strings.Contains(err.Error(), "destination_evidence") {
			t.Fatalf("%s missing destination evidence error = %v", cellID, err)
		}

		withDestination := missingDestination
		withDestination.ResultID = "destination-visible-" + cellID
		withDestination.ExactIdentityEvidence.DestinationEvidence = map[string]string{
			"destination":     "grok-peer/pdev/session-1",
			"acknowledgement": "ACK_" + cellID,
		}
		if err := ValidateResults(manifest, cells, Run{
			RequestedCellIDs: []string{cellID}, Results: append(append([]Result{}, base...), withDestination),
		}); err != nil {
			t.Fatalf("%s destination-visible evidence rejected: %v", cellID, err)
		}
	}
}

func TestMatrixRunnerRequiresEveryCellProductVersion(t *testing.T) {
	manifest, cells := loadResultContractManifest(t)
	base := []Result{
		validAcceptanceResult("source-pass", "S-01", "linux"),
		validAcceptanceResult("install-u01", "U-01", "linux"),
		validAcceptanceResult("install-u08", "U-08", "linux"),
	}
	result := validAcceptanceResult("message-missing-grok-version", "M-CP-GP", "linux")
	result.ExactIdentityEvidence.DestinationEvidence = map[string]string{
		"destination": "grok-peer/pdev/session-1", "acknowledgement": "ACK_M_CP_GP",
	}
	if err := ValidateResults(manifest, cells, Run{
		RequestedCellIDs: []string{"M-CP-GP"}, Results: append(append([]Result{}, base...), result),
	}); err == nil || !strings.Contains(err.Error(), "grok") {
		t.Fatalf("missing Grok version error = %v", err)
	}
}
