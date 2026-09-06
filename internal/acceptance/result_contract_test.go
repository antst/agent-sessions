package acceptance

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/antst/sessionbus/internal/releaseevidence"
)

func TestResultContractRejectsIncompleteOrMiscreditedRuns(t *testing.T) {
	manifest, cells := loadResultContractManifest(t)
	validSource := validAcceptanceResult("source-pass", "S-01", "linux")

	tests := []struct {
		name string
		run  Run
		want string
	}{
		{
			name: "unknown cell",
			run:  Run{RequestedCellIDs: []string{"UNKNOWN"}, Results: []Result{validAcceptanceResult("unknown", "UNKNOWN", "linux")}},
			want: "unknown cell",
		},
		{
			name: "missing requested cell result",
			run:  Run{RequestedCellIDs: []string{"S-01"}},
			want: "missing result",
		},
		{
			name: "duplicate credit",
			run: Run{RequestedCellIDs: []string{"S-01"}, Results: []Result{
				validSource,
				validAcceptanceResult("source-pass-again", "S-01", "linux"),
			}},
			want: "duplicate",
		},
		{
			name: "aggregate only output",
			run: Run{RequestedCellIDs: []string{"S-01"}, Results: []Result{
				validAcceptanceResult("aggregate", "ALL", "linux"),
			}},
			want: "aggregate",
		},
		{
			name: "missing prerequisite result",
			run: Run{RequestedCellIDs: []string{"U-01"}, Results: []Result{
				validAcceptanceResult("install-pass", "U-01", "linux"),
			}},
			want: "prerequisite",
		},
		{
			name: "prerequisite red omission",
			run: Run{RequestedCellIDs: []string{"U-01"}, Results: []Result{
				redAcceptanceResult("source-red", "S-01", "linux"),
			}},
			want: "NOT_EXECUTED_PREREQUISITE_RED",
		},
		{
			name: "fake vendor product credit",
			run: Run{RequestedCellIDs: []string{"C-01"}, Results: []Result{
				validSource,
				validAcceptanceResult("install-u01", "U-01", "linux"),
				validAcceptanceResult("install-u08", "U-08", "linux"),
				func() Result {
					result := validAcceptanceResult("codex-fake", "C-01", "linux")
					result.ExactIdentityEvidence = ExactIdentityEvidence{Kind: "fake-vendor", Product: "codex"}
					return result
				}(),
			}},
			want: "real installed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateResults(manifest, cells, test.run); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateResults() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestResultContractRequiresVerdictConditionalFields(t *testing.T) {
	manifest, cells := loadResultContractManifest(t)
	tests := []struct {
		verdict Verdict
		clear   func(*Result)
		want    string
	}{
		{VerdictPass, func(result *Result) { result.LiteralCommand = "" }, "literal_command"},
		{VerdictPass, func(result *Result) { result.ExitStatus = nil }, "exit_status"},
		{VerdictPass, func(result *Result) { status := 7; result.ExitStatus = &status }, "zero exit_status"},
		{VerdictPass, func(result *Result) { result.CleanupEvidence = "" }, "cleanup_evidence"},
		{VerdictNA, func(result *Result) { result.ApplicabilityEvidence = "" }, "applicability_evidence"},
		{VerdictBlocked, func(result *Result) { result.DiagnosticClassification = "" }, "diagnostic_classification"},
		{VerdictRed, func(result *Result) { result.PreservedStateEvidence = "" }, "preserved_state_evidence"},
		{VerdictNotExecutedPrerequisiteRed, func(result *Result) { result.FailedPrerequisiteIDs = nil }, "failed_prerequisite_ids"},
	}
	for _, test := range tests {
		t.Run(string(test.verdict)+"_missing_"+test.want, func(t *testing.T) {
			result := validAcceptanceResult("conditional", "S-01", "linux")
			result.Verdict = test.verdict
			result.VerdictReason = "attributable reason"
			result.DiagnosticClassification = "product"
			result.ApplicabilityEvidence = "capability absent"
			result.FailedPrerequisiteIDs = []string{"S-00"}
			test.clear(&result)
			run := Run{RequestedCellIDs: []string{"S-01"}, Results: []Result{result}}
			if err := ValidateResults(manifest, cells, run); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateResults() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestResultContractRequiresConcreteRealInstalledIdentity(t *testing.T) {
	manifest, cells := loadResultContractManifest(t)
	base := []Result{
		validAcceptanceResult("source-pass", "S-01", "linux"),
		validAcceptanceResult("install-u01", "U-01", "linux"),
		validAcceptanceResult("install-u08", "U-08", "linux"),
	}
	result := validAcceptanceResult("codex-no-native-identity", "C-01", "linux")
	result.ExactIdentityEvidence = ExactIdentityEvidence{Kind: "real-installed", Product: "codex"}
	if err := ValidateResults(manifest, cells, Run{
		RequestedCellIDs: []string{"C-01"}, Results: append(append([]Result{}, base...), result),
	}); err == nil || !strings.Contains(err.Error(), "concrete native identity") {
		t.Fatalf("missing concrete native identity error = %v", err)
	}
}

func TestResultContractRequiresExplicitExactSupersession(t *testing.T) {
	manifest, cells := loadResultContractManifest(t)
	prior := validAcceptanceResult("prior", "S-01", "linux")
	rerun := validAcceptanceResult("rerun", "S-01", "linux")
	if err := ValidateResults(manifest, cells, Run{
		RequestedCellIDs: []string{"S-01"}, Results: []Result{prior, rerun},
	}); err == nil || !strings.Contains(err.Error(), "supersedes_result_id") {
		t.Fatalf("ambiguous rerun error = %v", err)
	}

	rerun.SupersedesResultID = prior.ResultID
	if err := ValidateResults(manifest, cells, Run{
		RequestedCellIDs: []string{"S-01"}, Results: []Result{prior, rerun},
	}); err != nil {
		t.Fatalf("exact supersession rejected: %v", err)
	}

	for _, mutate := range []func(*Result){
		func(result *Result) { result.CellID = "S-02" },
		func(result *Result) { result.SourceCommit = "different-candidate" },
		func(result *Result) { result.OS = "macos" },
	} {
		changed := rerun
		mutate(&changed)
		if err := ValidateResults(manifest, cells, Run{
			RequestedCellIDs: []string{"S-01"}, Results: []Result{prior, changed},
		}); err == nil || !strings.Contains(err.Error(), "same cell, candidate, and platform") {
			t.Fatalf("cross-identity supersession error = %v", err)
		}
	}
}

func loadResultContractManifest(t *testing.T) (*releaseevidence.AcceptanceManifest, []releaseevidence.AcceptanceCell) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve acceptance contract test path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	manifest, err := releaseevidence.LoadAcceptanceManifest(filepath.Join(root, "specs/002-unified-user-daemon/contracts/acceptance-matrix.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cells, err := manifest.ValidateAndExpand(root)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, cells
}

func validAcceptanceResult(resultID, cellID, platform string) Result {
	exitStatus := 0
	return Result{
		ResultID: resultID, CellID: cellID, Verdict: VerdictPass,
		SourceCommit: "candidate-commit", SourceTree: "candidate-tree",
		InstalledReleaseIdentity: "sha256:candidate", OS: platform, Architecture: "x86_64",
		NativeProductVersions: map[string]string{"codex": "real-version"}, EvidencePaths: []string{"evidence/result.json"},
		LiteralCommand: "exact command", Cwd: "/exact/cwd", ExitStatus: &exitStatus,
		ExactIdentityEvidence: ExactIdentityEvidence{
			Kind: "real-installed", Product: "codex", ArtifactIdentity: "sha256:real-native-artifact",
		},
		PreservedStateEvidence: "preserved", CleanupEvidence: "clean",
	}
}

func redAcceptanceResult(resultID, cellID, platform string) Result {
	result := validAcceptanceResult(resultID, cellID, platform)
	result.Verdict = VerdictRed
	result.VerdictReason = "product assertion failed"
	result.DiagnosticClassification = "product"
	return result
}
