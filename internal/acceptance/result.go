// Package acceptance validates attributable executions of the closed acceptance matrix.
package acceptance

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/antst/sessionbus/internal/releaseevidence"
)

// Verdict is the exact outcome of one acceptance cell execution.
type Verdict string

const (
	// VerdictPass grants credit for a fully evidenced successful cell.
	VerdictPass Verdict = "PASS"
	// VerdictNA records proven capability-based non-applicability.
	VerdictNA Verdict = "N/A"
	// VerdictBlocked records an attributable external or environment blocker.
	VerdictBlocked Verdict = "BLOCKED"
	// VerdictRed records a genuine product, harness, or safety failure.
	VerdictRed Verdict = "RED"
	// VerdictNotExecutedPrerequisiteRed propagates one declared failed prerequisite.
	VerdictNotExecutedPrerequisiteRed Verdict = "NOT_EXECUTED_PREREQUISITE_RED"
)

// ExactIdentityEvidence identifies the real native subject observed by a cell.
// Values are identifiers and metadata only; content and credentials do not belong here.
type ExactIdentityEvidence struct {
	Kind                string            `json:"kind" yaml:"kind"`
	Product             string            `json:"product,omitempty" yaml:"product,omitempty"`
	NativeSessionID     string            `json:"native_session_id,omitempty" yaml:"native_session_id,omitempty"`
	ProcessIdentity     string            `json:"process_identity,omitempty" yaml:"process_identity,omitempty"`
	ArtifactIdentity    string            `json:"artifact_identity,omitempty" yaml:"artifact_identity,omitempty"`
	DestinationEvidence map[string]string `json:"destination_evidence,omitempty" yaml:"destination_evidence,omitempty"`
}

// Result is one attributable execution result for one cell, candidate, and platform.
type Result struct {
	ResultID                   string                `json:"result_id" yaml:"result_id"`
	CellID                     string                `json:"cell_id" yaml:"cell_id"`
	Verdict                    Verdict               `json:"verdict" yaml:"verdict"`
	VerdictReason              string                `json:"verdict_reason,omitempty" yaml:"verdict_reason,omitempty"`
	DiagnosticClassification   string                `json:"diagnostic_classification,omitempty" yaml:"diagnostic_classification,omitempty"`
	ApplicabilityEvidence      string                `json:"applicability_evidence,omitempty" yaml:"applicability_evidence,omitempty"`
	FailedPrerequisiteIDs      []string              `json:"failed_prerequisite_ids,omitempty" yaml:"failed_prerequisite_ids,omitempty"`
	SourceCommit               string                `json:"source_commit" yaml:"source_commit"`
	SourceTree                 string                `json:"source_tree" yaml:"source_tree"`
	InstalledReleaseIdentity   string                `json:"installed_release_identity" yaml:"installed_release_identity"`
	OS                         string                `json:"os" yaml:"os"`
	Architecture               string                `json:"architecture" yaml:"architecture"`
	NativeProductVersions      map[string]string     `json:"native_product_versions" yaml:"native_product_versions"`
	EvidencePaths              []string              `json:"evidence_paths" yaml:"evidence_paths"`
	LiteralCommand             string                `json:"literal_command,omitempty" yaml:"literal_command,omitempty"`
	Cwd                        string                `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	RelevantEnvironmentPresent map[string]bool       `json:"relevant_environment_presence,omitempty" yaml:"relevant_environment_presence,omitempty"`
	ExitStatus                 *int                  `json:"exit_status,omitempty" yaml:"exit_status,omitempty"`
	ExactIdentityEvidence      ExactIdentityEvidence `json:"exact_identity_evidence,omitempty" yaml:"exact_identity_evidence,omitempty"`
	PreservedStateEvidence     string                `json:"preserved_state_evidence,omitempty" yaml:"preserved_state_evidence,omitempty"`
	CleanupEvidence            string                `json:"cleanup_evidence,omitempty" yaml:"cleanup_evidence,omitempty"`
	SupersedesResultID         string                `json:"supersedes_result_id,omitempty" yaml:"supersedes_result_id,omitempty"`
}

// Run is one exact requested-cell invocation and all prerequisite/cell results it emitted.
type Run struct {
	RequestedCellIDs []string `json:"requested_cell_ids" yaml:"requested_cell_ids"`
	Results          []Result `json:"results" yaml:"results"`
}

// ValidateResults rejects aggregate, incomplete, misattributed, or ambiguous acceptance credit.
//
//nolint:gocyclo // The closed contract deliberately validates every credit and supersession branch.
func ValidateResults(manifest *releaseevidence.AcceptanceManifest, cells []releaseevidence.AcceptanceCell, run Run) error {
	if manifest == nil {
		return errors.New("acceptance manifest is nil")
	}
	cellByID := make(map[string]releaseevidence.AcceptanceCell, len(cells))
	for _, cell := range cells {
		if _, exists := cellByID[cell.ID]; exists {
			return fmt.Errorf("duplicate acceptance cell %q", cell.ID)
		}
		cellByID[cell.ID] = cell
	}
	if len(run.RequestedCellIDs) == 0 {
		return errors.New("requested cell inventory is empty")
	}
	requested := make(map[string]struct{}, len(run.RequestedCellIDs))
	for _, id := range run.RequestedCellIDs {
		if _, ok := cellByID[id]; !ok {
			return fmt.Errorf("unknown cell %q was requested", id)
		}
		if _, duplicate := requested[id]; duplicate {
			return fmt.Errorf("duplicate requested cell %q", id)
		}
		requested[id] = struct{}{}
	}

	resultByID := make(map[string]*Result, len(run.Results))
	for index := range run.Results {
		result := &run.Results[index]
		if strings.EqualFold(result.CellID, "ALL") || result.CellID == "*" {
			return errors.New("aggregate-only output cannot receive acceptance credit")
		}
		if err := validateResultFields(manifest, cellByID, *result); err != nil {
			return fmt.Errorf("result %q: %w", result.ResultID, err)
		}
		if _, duplicate := resultByID[result.ResultID]; duplicate {
			return fmt.Errorf("duplicate result_id %q", result.ResultID)
		}
		resultByID[result.ResultID] = result
	}

	superseded := make(map[string]string)
	for index := range run.Results {
		result := &run.Results[index]
		if result.SupersedesResultID == "" {
			continue
		}
		prior, ok := resultByID[result.SupersedesResultID]
		if !ok || prior.ResultID == result.ResultID {
			return fmt.Errorf("result %q supersedes unknown prior result %q", result.ResultID, result.SupersedesResultID)
		}
		if !sameCellCandidatePlatform(*prior, *result) {
			return fmt.Errorf("result %q may supersede only the same cell, candidate, and platform", result.ResultID)
		}
		if next, duplicate := superseded[prior.ResultID]; duplicate {
			return fmt.Errorf("prior result %q is superseded by both %q and %q", prior.ResultID, next, result.ResultID)
		}
		superseded[prior.ResultID] = result.ResultID
	}
	for _, result := range run.Results {
		if !contains(cellByID[result.CellID].Platforms, result.OS) {
			return fmt.Errorf("result %q: os %q is outside cell platform scope", result.ResultID, result.OS)
		}
	}

	authoritative := make([]Result, 0, len(run.Results))
	for _, result := range run.Results {
		if _, isSuperseded := superseded[result.ResultID]; !isSuperseded {
			authoritative = append(authoritative, result)
		}
	}
	for i := range authoritative {
		for j := i + 1; j < len(authoritative); j++ {
			if sameCellCandidatePlatform(authoritative[i], authoritative[j]) {
				return fmt.Errorf("duplicate credit for cell %q requires explicit supersedes_result_id", authoritative[i].CellID)
			}
		}
	}

	allowed := prerequisiteClosure(cellByID, requested)
	for _, result := range authoritative {
		if _, ok := allowed[result.CellID]; !ok {
			return fmt.Errorf("result for unrequested non-prerequisite cell %q", result.CellID)
		}
	}

	for requestedID := range requested {
		cell := cellByID[requestedID]
		targets := authoritativeForCell(authoritative, requestedID)
		if len(targets) == 0 {
			failed := failedPrerequisitesForMissingTarget(authoritative, cell.Prerequisites)
			if len(failed) != 0 {
				return fmt.Errorf("cell %q requires a %s result for failed prerequisite(s) %s", requestedID, VerdictNotExecutedPrerequisiteRed, strings.Join(failed, ","))
			}
			return fmt.Errorf("missing result for requested cell %q", requestedID)
		}
		for _, target := range targets {
			failed := make([]string, 0)
			for _, prerequisiteID := range cell.Prerequisites {
				prerequisite, ok := matchingAuthoritative(authoritative, prerequisiteID, target)
				if !ok {
					return fmt.Errorf("missing prerequisite result %q for cell %q", prerequisiteID, requestedID)
				}
				if prerequisite.Verdict == VerdictRed || prerequisite.Verdict == VerdictBlocked || prerequisite.Verdict == VerdictNotExecutedPrerequisiteRed {
					failed = append(failed, prerequisiteID)
				}
			}
			sort.Strings(failed)
			if len(failed) != 0 {
				if target.Verdict != VerdictNotExecutedPrerequisiteRed || !equalStringSets(target.FailedPrerequisiteIDs, failed) {
					return fmt.Errorf("cell %q must be %s with failed_prerequisite_ids %v", requestedID, VerdictNotExecutedPrerequisiteRed, failed)
				}
			} else if target.Verdict == VerdictNotExecutedPrerequisiteRed {
				return fmt.Errorf("cell %q reports %s without a RED prerequisite", requestedID, VerdictNotExecutedPrerequisiteRed)
			}
		}
	}
	return nil
}

//nolint:gocyclo // Verdict-conditional fields are intentionally explicit and auditable.
func validateResultFields(manifest *releaseevidence.AcceptanceManifest, cells map[string]releaseevidence.AcceptanceCell, result Result) error {
	if strings.TrimSpace(result.ResultID) == "" {
		return errors.New("result_id is required")
	}
	cell, known := cells[result.CellID]
	if !known {
		return fmt.Errorf("unknown cell %q", result.CellID)
	}
	if !contains(manifest.Verdicts, string(result.Verdict)) {
		return fmt.Errorf("verdict %q is not allowed", result.Verdict)
	}
	if strings.TrimSpace(result.SourceCommit) == "" {
		return errors.New("source_commit is required")
	}
	if strings.TrimSpace(result.SourceTree) == "" {
		return errors.New("source_tree is required")
	}
	if strings.TrimSpace(result.InstalledReleaseIdentity) == "" {
		return errors.New("installed_release_identity is required")
	}
	if strings.TrimSpace(result.OS) == "" {
		return errors.New("os is required")
	}
	if strings.TrimSpace(result.Architecture) == "" {
		return errors.New("architecture is required")
	}
	if len(result.NativeProductVersions) == 0 {
		return errors.New("native_product_versions is required")
	}
	if !nonblankStringMap(result.NativeProductVersions) {
		return errors.New("native_product_versions must contain only nonblank product/version pairs")
	}
	for _, product := range releaseevidence.ProductsForAcceptanceCell(cell) {
		if strings.TrimSpace(result.NativeProductVersions[product]) == "" {
			return fmt.Errorf("native_product_versions requires %s for cell %s", product, cell.ID)
		}
	}
	if len(result.EvidencePaths) == 0 || containsBlank(result.EvidencePaths) {
		return errors.New("evidence_paths is required")
	}

	require := func(ok bool, field string) error {
		if !ok {
			return fmt.Errorf("%s is required for %s", field, result.Verdict)
		}
		return nil
	}
	switch result.Verdict {
	case VerdictPass:
		if err := require(strings.TrimSpace(result.LiteralCommand) != "", "literal_command"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.Cwd) != "", "cwd"); err != nil {
			return err
		}
		if err := require(result.ExitStatus != nil, "exit_status"); err != nil {
			return err
		}
		if err := require(*result.ExitStatus == 0, "zero exit_status"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.ExactIdentityEvidence.Kind) != "", "exact_identity_evidence"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.PreservedStateEvidence) != "", "preserved_state_evidence"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.CleanupEvidence) != "", "cleanup_evidence"); err != nil {
			return err
		}
	case VerdictNA:
		if err := require(strings.TrimSpace(result.VerdictReason) != "", "verdict_reason"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.ApplicabilityEvidence) != "", "applicability_evidence"); err != nil {
			return err
		}
	case VerdictBlocked:
		if err := require(strings.TrimSpace(result.VerdictReason) != "", "verdict_reason"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.DiagnosticClassification) != "", "diagnostic_classification"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.ApplicabilityEvidence) != "", "applicability_evidence"); err != nil {
			return err
		}
	case VerdictRed:
		if err := require(strings.TrimSpace(result.VerdictReason) != "", "verdict_reason"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.DiagnosticClassification) != "", "diagnostic_classification"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.LiteralCommand) != "", "literal_command"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.Cwd) != "", "cwd"); err != nil {
			return err
		}
		if err := require(result.ExitStatus != nil, "exit_status"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.ExactIdentityEvidence.Kind) != "", "exact_identity_evidence"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.PreservedStateEvidence) != "", "preserved_state_evidence"); err != nil {
			return err
		}
		if err := require(strings.TrimSpace(result.CleanupEvidence) != "", "cleanup_evidence"); err != nil {
			return err
		}
	case VerdictNotExecutedPrerequisiteRed:
		if err := require(strings.TrimSpace(result.VerdictReason) != "", "verdict_reason"); err != nil {
			return err
		}
		if err := require(len(result.FailedPrerequisiteIDs) != 0 && !containsBlank(result.FailedPrerequisiteIDs), "failed_prerequisite_ids"); err != nil {
			return err
		}
	}
	if requiresRealInstalledEvidence(cell) && (result.Verdict == VerdictPass || result.Verdict == VerdictRed) {
		if result.ExactIdentityEvidence.Kind != "real-installed" {
			return errors.New("real installed native product evidence is required")
		}
		if strings.TrimSpace(result.ExactIdentityEvidence.Product) == "" {
			return errors.New("real installed native product evidence requires product")
		}
		if strings.TrimSpace(result.ExactIdentityEvidence.NativeSessionID) == "" &&
			strings.TrimSpace(result.ExactIdentityEvidence.ProcessIdentity) == "" &&
			strings.TrimSpace(result.ExactIdentityEvidence.ArtifactIdentity) == "" {
			return errors.New("real installed native product evidence requires a concrete native identity")
		}
	}
	if requiresDestinationVisibleEvidence(cell) && (result.Verdict == VerdictPass || result.Verdict == VerdictRed) &&
		!nonblankStringMap(result.ExactIdentityEvidence.DestinationEvidence) {
		return errors.New("destination_evidence with nonblank destination-visible receipt fields is required")
	}
	return nil
}

func requiresRealInstalledEvidence(cell releaseevidence.AcceptanceCell) bool {
	switch cell.Family {
	case "codex-interactive", "claude-interactive", "grok-interactive", "qwen-interactive", "lane-lifecycle", "parent-target-composition", "peer-lane-messaging", "archive-unarchive":
		return true
	default:
		return false
	}
}

func requiresDestinationVisibleEvidence(cell releaseevidence.AcceptanceCell) bool {
	return cell.Family == "parent-target-composition" || cell.Family == "peer-lane-messaging"
}

func nonblankStringMap(values map[string]string) bool {
	if len(values) == 0 {
		return false
	}
	for key, value := range values {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func prerequisiteClosure(cells map[string]releaseevidence.AcceptanceCell, requested map[string]struct{}) map[string]struct{} {
	closure := make(map[string]struct{}, len(requested))
	var add func(string)
	add = func(id string) {
		if _, seen := closure[id]; seen {
			return
		}
		closure[id] = struct{}{}
		for _, prerequisite := range cells[id].Prerequisites {
			add(prerequisite)
		}
	}
	for id := range requested {
		add(id)
	}
	return closure
}

func authoritativeForCell(results []Result, cellID string) []Result {
	var matched []Result
	for _, result := range results {
		if result.CellID == cellID {
			matched = append(matched, result)
		}
	}
	return matched
}

func matchingAuthoritative(results []Result, cellID string, target Result) (Result, bool) {
	for _, result := range results {
		if result.CellID == cellID && sameCandidatePlatform(result, target) {
			return result, true
		}
	}
	return Result{}, false
}

func failedPrerequisitesForMissingTarget(results []Result, prerequisiteIDs []string) []string {
	var failed []string
	for _, id := range prerequisiteIDs {
		for _, result := range results {
			if result.CellID == id && (result.Verdict == VerdictRed || result.Verdict == VerdictBlocked || result.Verdict == VerdictNotExecutedPrerequisiteRed) {
				failed = append(failed, id)
				break
			}
		}
	}
	sort.Strings(failed)
	return failed
}

func sameCellCandidatePlatform(left, right Result) bool {
	return left.CellID == right.CellID && sameCandidatePlatform(left, right)
}

func sameCandidatePlatform(left, right Result) bool {
	return left.SourceCommit == right.SourceCommit && left.SourceTree == right.SourceTree &&
		left.InstalledReleaseIdentity == right.InstalledReleaseIdentity && left.OS == right.OS &&
		left.Architecture == right.Architecture && reflect.DeepEqual(left.NativeProductVersions, right.NativeProductVersions)
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}
