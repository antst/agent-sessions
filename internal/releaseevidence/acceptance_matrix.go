package releaseevidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxAcceptanceManifestBytes = 1 << 20

// AcceptanceManifest is the authoritative closed acceptance-cell inventory.
type AcceptanceManifest struct {
	SchemaVersion         int                      `yaml:"schema_version"`
	BaselineCommit        string                   `yaml:"baseline_commit"`
	AssertionSource       string                   `yaml:"assertion_source"`
	ExpectedCellCount     int                      `yaml:"expected_cell_count"`
	CellExpansionContract CellExpansionContract    `yaml:"cell_expansion_contract"`
	TopologyDeltaContract TopologyDeltaContract    `yaml:"topology_delta_contract"`
	Verdicts              []string                 `yaml:"verdicts"`
	ResultContract        AcceptanceResultContract `yaml:"result_contract"`
	TopologyDeltas        map[string]TopologyDelta `yaml:"topology_deltas"`
	Families              []AcceptanceFamily       `yaml:"families"`
}

// CellExpansionContract declares the checked schema used to expand families.
type CellExpansionContract struct {
	RequiredFamilyFields   []string                  `yaml:"required_family_fields"`
	RequiredExpandedFields []string                  `yaml:"required_expanded_fields"`
	EvidenceTiers          EvidenceTierContract      `yaml:"evidence_tiers"`
	AssertionLocator       AssertionLocatorContract  `yaml:"assertion_locator"`
	PlatformScope          PlatformScopeContract     `yaml:"platform_scope"`
	PrerequisiteGraph      PrerequisiteGraphContract `yaml:"prerequisite_graph"`
}

// EvidenceTierContract bounds accepted evidence tiers.
type EvidenceTierContract struct {
	Allowed         []string `yaml:"allowed"`
	RequireNonempty bool     `yaml:"require_nonempty"`
}

// AssertionLocatorContract defines the authoritative Markdown locator.
type AssertionLocatorContract struct {
	Document                       string `yaml:"document"`
	SectionField                   string `yaml:"section_field"`
	LocatorTemplateField           string `yaml:"locator_template_field"`
	RequireExactlyOneCellInSection bool   `yaml:"require_exactly_one_cell_id_in_section"`
}

// PlatformScopeContract bounds platform expansion and optional applicability.
type PlatformScopeContract struct {
	Allowed                       []string `yaml:"allowed"`
	RequireNonemptyAfterOverrides bool     `yaml:"require_nonempty_after_overrides"`
	ApplicabilityField            string   `yaml:"applicability_field"`
}

// PrerequisiteGraphContract declares the explicit dependency graph rules.
type PrerequisiteGraphContract struct {
	References                string `yaml:"references"`
	Acyclic                   bool   `yaml:"acyclic"`
	NoImplicitDependencies    bool   `yaml:"no_implicit_dependencies"`
	MissingPrerequisiteResult string `yaml:"missing_prerequisite_result"`
	FailedPrerequisiteVerdict string `yaml:"failed_prerequisite_verdict"`
}

// TopologyDeltaContract bounds permitted assertion substitutions.
type TopologyDeltaContract struct {
	Default                          string   `yaml:"default"`
	AllowedScope                     string   `yaml:"allowed_scope"`
	RequiredFields                   []string `yaml:"required_fields"`
	RequireKnownCellID               bool     `yaml:"require_known_cell_id"`
	RequireTargetAssertionLocator    bool     `yaml:"require_target_assertion_locator"`
	FunctionalOrNativeBehaviorChange string   `yaml:"functional_or_native_product_behavior_change"`
}

// AcceptanceResultContract describes result fields and supersession rules.
type AcceptanceResultContract struct {
	RequiredFields    []string             `yaml:"required_fields"`
	ConditionalFields map[string][]string  `yaml:"conditional_fields"`
	AggregateCredit   string               `yaml:"aggregate_credit"`
	DuplicateCredit   string               `yaml:"duplicate_credit"`
	Supersession      SupersessionContract `yaml:"supersession"`
}

// SupersessionContract bounds authoritative reruns.
type SupersessionContract struct {
	Allowed                             bool `yaml:"allowed"`
	PriorResultReceivesCredit           bool `yaml:"prior_result_receives_credit"`
	RequireSameCellCandidateAndPlatform bool `yaml:"require_same_cell_candidate_and_platform"`
}

// TopologyDelta is one reviewed Agent Sessions-only observation change.
type TopologyDelta struct {
	BaselineObservation string `yaml:"baseline_observation"`
	TargetObservation   string `yaml:"target_observation"`
	PreservedInvariant  string `yaml:"preserved_invariant"`
}

// AcceptanceFamily is the compact manifest representation of related cells.
type AcceptanceFamily struct {
	ID              string                            `yaml:"id"`
	EvidenceTiers   []string                          `yaml:"evidence_tiers"`
	Platforms       []string                          `yaml:"platforms"`
	AssertionSource AcceptanceAssertionSource         `yaml:"assertion_source"`
	Prerequisites   []string                          `yaml:"prerequisites"`
	CellOverrides   map[string]AcceptanceCellOverride `yaml:"cell_overrides"`
	IDs             []string                          `yaml:"ids"`
}

// AcceptanceAssertionSource identifies one exact Markdown section and locator form.
type AcceptanceAssertionSource struct {
	Section         string `yaml:"section"`
	LocatorTemplate string `yaml:"locator_template"`
}

// AcceptanceCellOverride narrows platform or capability applicability for one cell.
type AcceptanceCellOverride struct {
	Platforms     []string `yaml:"platforms"`
	Applicability string   `yaml:"applicability"`
}

// AcceptanceCell is one fully expanded member of the closed matrix.
type AcceptanceCell struct {
	ID              string
	Family          string
	EvidenceTiers   []string
	Platforms       []string
	AssertionSource AcceptanceAssertionSource
	Prerequisites   []string
	Applicability   string
	TopologyDelta   *TopologyDelta
}

// LoadAcceptanceManifest parses a bounded manifest and rejects unknown fields.
func LoadAcceptanceManifest(path string) (*AcceptanceManifest, error) {
	return loadBoundedYAML[AcceptanceManifest](path, "acceptance manifest", maxAcceptanceManifestBytes)
}

// ValidateAndExpand validates the closed manifest and returns all expanded cells.
//
//nolint:gocyclo // The closed acceptance contract validates each independent manifest invariant explicitly.
func (manifest *AcceptanceManifest) ValidateAndExpand(repositoryRoot string) ([]AcceptanceCell, error) {
	if manifest == nil || manifest.SchemaVersion != 1 || manifest.ExpectedCellCount <= 0 ||
		manifest.BaselineCommit == "" || manifest.AssertionSource == "" {
		return nil, errors.New("acceptance manifest identity is incomplete")
	}
	allowedTiers := stringSet(manifest.CellExpansionContract.EvidenceTiers.Allowed)
	allowedPlatforms := stringSet(manifest.CellExpansionContract.PlatformScope.Allowed)
	if len(allowedTiers) == 0 || len(allowedPlatforms) == 0 || len(manifest.Families) == 0 {
		return nil, errors.New("acceptance expansion contract is incomplete")
	}

	cells := make([]AcceptanceCell, 0, manifest.ExpectedCellCount)
	byID := make(map[string]int, manifest.ExpectedCellCount)
	for _, family := range manifest.Families {
		if err := expandAcceptanceFamily(family, allowedTiers, allowedPlatforms, &cells, byID); err != nil {
			return nil, err
		}
	}
	if len(cells) != manifest.ExpectedCellCount {
		return nil, fmt.Errorf("expanded acceptance cell count = %d, want %d", len(cells), manifest.ExpectedCellCount)
	}
	for index := range cells {
		cell := &cells[index]
		for _, prerequisite := range cell.Prerequisites {
			if byID[prerequisite] == 0 {
				return nil, fmt.Errorf("acceptance cell %s references unknown prerequisite %s", cell.ID, prerequisite)
			}
		}
		if delta, ok := manifest.TopologyDeltas[cell.ID]; ok {
			deltaCopy := delta
			cell.TopologyDelta = &deltaCopy
		}
	}
	for id, delta := range manifest.TopologyDeltas {
		if byID[id] == 0 {
			return nil, fmt.Errorf("topology delta references unknown acceptance cell %s", id)
		}
		if delta.BaselineObservation == "" || delta.TargetObservation == "" || delta.PreservedInvariant == "" {
			return nil, fmt.Errorf("topology delta %s is incomplete", id)
		}
	}
	if err := validateAcceptanceDAG(cells); err != nil {
		return nil, err
	}
	if err := validateAcceptanceAssertions(repositoryRoot, manifest, cells); err != nil {
		return nil, err
	}
	return cells, nil
}

//nolint:gocyclo // Family expansion validates tiers, overrides, platforms, and stable IDs independently.
func expandAcceptanceFamily(
	family AcceptanceFamily,
	allowedTiers, allowedPlatforms map[string]bool,
	cells *[]AcceptanceCell,
	byID map[string]int,
) error {
	if family.ID == "" || len(family.IDs) == 0 || family.AssertionSource.Section == "" ||
		!strings.Contains(family.AssertionSource.LocatorTemplate, "{cell_id}") {
		return fmt.Errorf("acceptance family %q is incomplete", family.ID)
	}
	if len(family.EvidenceTiers) == 0 {
		return fmt.Errorf("acceptance family %s has empty evidence tiers", family.ID)
	}
	for _, tier := range family.EvidenceTiers {
		if !allowedTiers[tier] {
			return fmt.Errorf("acceptance family %s has unknown evidence tier %q", family.ID, tier)
		}
	}
	for overrideID := range family.CellOverrides {
		if !containsString(family.IDs, overrideID) {
			return fmt.Errorf("acceptance family %s overrides unknown cell %s", family.ID, overrideID)
		}
	}
	for _, id := range family.IDs {
		if id == "" || byID[id] != 0 {
			return fmt.Errorf("duplicate acceptance cell %q", id)
		}
		platforms := append([]string(nil), family.Platforms...)
		override, overridden := family.CellOverrides[id]
		if overridden && override.Platforms != nil {
			platforms = append([]string(nil), override.Platforms...)
		}
		if len(platforms) == 0 {
			return fmt.Errorf("acceptance cell %s has empty platform scope", id)
		}
		for _, platform := range platforms {
			if !allowedPlatforms[platform] {
				return fmt.Errorf("acceptance cell %s has unknown platform %q", id, platform)
			}
		}
		*cells = append(*cells, AcceptanceCell{
			ID: id, Family: family.ID,
			EvidenceTiers: append([]string(nil), family.EvidenceTiers...),
			Platforms:     platforms, AssertionSource: family.AssertionSource,
			Prerequisites: append([]string(nil), family.Prerequisites...),
			Applicability: override.Applicability,
		})
		byID[id] = len(*cells)
	}
	return nil
}

func validateAcceptanceDAG(cells []AcceptanceCell) error {
	graph := make(map[string][]string, len(cells))
	for _, cell := range cells {
		graph[cell.ID] = cell.Prerequisites
	}
	state := make(map[string]uint8, len(cells))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("acceptance prerequisite cycle includes %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, next := range graph[id] {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(graph))
	for id := range graph {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validateAcceptanceAssertions(repositoryRoot string, manifest *AcceptanceManifest, cells []AcceptanceCell) error {
	if filepath.IsAbs(manifest.AssertionSource) ||
		filepath.Clean(filepath.FromSlash(manifest.AssertionSource)) != filepath.FromSlash(manifest.AssertionSource) {
		return errors.New("acceptance assertion source is not a clean repository path")
	}
	path := filepath.Join(repositoryRoot, filepath.FromSlash(manifest.AssertionSource))
	// #nosec G304 -- the clean relative source is joined beneath the repository root above.
	body, err := os.ReadFile(path) // public repository documentation.
	if err != nil {
		return fmt.Errorf("read acceptance assertion source: %w", err)
	}
	document := string(body)
	sections := make(map[string]string)
	for _, cell := range cells {
		section, ok := sections[cell.AssertionSource.Section]
		if !ok {
			section, err = markdownSection(document, cell.AssertionSource.Section)
			if err != nil {
				return err
			}
			sections[cell.AssertionSource.Section] = section
		}
		count := markdownTableCellCount(section, cell.ID)
		if count != 1 {
			return fmt.Errorf("acceptance assertion locator for %s resolves %d times in section %q",
				cell.ID, count, cell.AssertionSource.Section)
		}
	}
	return nil
}

func markdownSection(document, title string) (string, error) {
	heading := "## " + title
	start := strings.Index(document, heading)
	if start < 0 || (start > 0 && document[start-1] != '\n') {
		return "", fmt.Errorf("acceptance assertion section %q is missing", title)
	}
	rest := document[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest, nil
}

func markdownTableCellCount(section, wanted string) int {
	count := 0
	for _, line := range strings.Split(section, "\n") {
		if !strings.Contains(line, "|") {
			continue
		}
		for _, cell := range strings.Split(line, "|") {
			if strings.TrimSpace(cell) == wanted {
				count++
			}
		}
	}
	return count
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
