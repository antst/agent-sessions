package releaseevidence

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const maxBaselinePortMapBytes = 1 << 20

// BaselinePortMap binds each legacy behavior owner to its replacement evidence.
type BaselinePortMap struct {
	SchemaVersion      int                   `yaml:"schema_version"`
	ManifestRevision   int                   `yaml:"manifest_revision"`
	BaselineCommit     string                `yaml:"baseline_commit"`
	AcceptanceManifest string                `yaml:"acceptance_manifest"`
	StatusOrder        []string              `yaml:"status_order"`
	StatusContract     PortMapStatusContract `yaml:"status_contract"`
	TraceabilityGate   PortMapTraceability   `yaml:"traceability_gate"`
	ImplementationGate PortMapGate           `yaml:"implementation_gate"`
	DeletionGate       PortMapGate           `yaml:"deletion_gate"`
	Entries            []BaselinePortEntry   `yaml:"entries"`
}

// PortMapStatusContract defines cumulative current-status validation.
type PortMapStatusContract struct {
	Storage                                    string                      `yaml:"storage"`
	MonotonicAcrossManifestRevisions           bool                        `yaml:"monotonic_across_manifest_revisions"`
	Validation                                 string                      `yaml:"validation"`
	EvidenceField                              string                      `yaml:"evidence_field"`
	CurrentStatusMustSatisfyAllApplicableGates bool                        `yaml:"current_status_must_satisfy_all_prior_applicable_gates"`
	ScalarStatusHistoryRequired                bool                        `yaml:"scalar_status_history_required"`
	StageGates                                 map[string]PortMapStageGate `yaml:"stage_gates"`
}

// PortMapStageGate names the fields required upon reaching one status.
type PortMapStageGate struct {
	RequiredNonemptyFields []string `yaml:"required_nonempty_fields"`
}

// PortMapTraceability defines the closed acceptance-cell coverage gate.
type PortMapTraceability struct {
	RequireAcceptanceManifestReferences bool `yaml:"require_acceptance_manifest_references"`
	RequireCompleteAcceptanceUnion      bool `yaml:"require_complete_acceptance_union"`
}

// PortMapGate is a minimum-status gate over selected entries.
type PortMapGate struct {
	Scope                  string   `yaml:"scope,omitempty"`
	MinimumStatus          string   `yaml:"minimum_status"`
	RequiredNonemptyFields []string `yaml:"required_nonempty_fields"`
}

// BaselinePortEntry maps one legacy behavior family to replacement ownership.
type BaselinePortEntry struct {
	ID               string   `yaml:"id"`
	Product          string   `yaml:"product"`
	BaselinePaths    []string `yaml:"baseline_paths"`
	OldSymbols       []string `yaml:"old_symbols"`
	OldTests         []string `yaml:"old_tests"`
	Invariant        string   `yaml:"invariant"`
	NewOwner         string   `yaml:"new_owner"`
	NewSymbols       []string `yaml:"new_symbols"`
	ReplacementTests []string `yaml:"replacement_tests"`
	AcceptanceCells  []string `yaml:"acceptance_cells"`
	Evidence         []string `yaml:"evidence"`
	DeletionPaths    []string `yaml:"deletion_paths"`
	Status           string   `yaml:"status"`
}

// LoadBaselinePortMap parses a bounded public contract and rejects unknown fields.
func LoadBaselinePortMap(path string) (*BaselinePortMap, error) {
	return loadBoundedYAML[BaselinePortMap](path, "baseline port map", maxBaselinePortMapBytes)
}

// Validate checks schema, cumulative status gates, traceability, and revision progress.
//
//nolint:gocyclo // The closed port map validates every stage and traceability invariant explicitly.
func (portMap *BaselinePortMap) Validate(cells []AcceptanceCell, previous *BaselinePortMap) error {
	if err := portMap.validateContract(); err != nil {
		return err
	}
	statusIndex := orderedIndex(portMap.StatusOrder)
	knownCells := make(map[string]bool, len(cells))
	for _, cell := range cells {
		if strings.TrimSpace(cell.ID) == "" || knownCells[cell.ID] {
			return fmt.Errorf("acceptance inventory contains duplicate or empty cell %q", cell.ID)
		}
		knownCells[cell.ID] = true
	}

	coveredCells := make(map[string]bool, len(cells))
	entries := make(map[string]BaselinePortEntry, len(portMap.Entries))
	for _, entry := range portMap.Entries {
		if strings.TrimSpace(entry.ID) == "" || entries[entry.ID].ID != "" {
			return fmt.Errorf("baseline port map contains duplicate or empty entry %q", entry.ID)
		}
		entries[entry.ID] = entry
		currentStage, ok := statusIndex[entry.Status]
		if !ok {
			return fmt.Errorf("baseline port-map entry %s has unknown status %q", entry.ID, entry.Status)
		}
		for stage := 0; stage <= currentStage; stage++ {
			stageName := portMap.StatusOrder[stage]
			for _, field := range portMap.StatusContract.StageGates[stageName].RequiredNonemptyFields {
				if !portEntryFieldNonempty(entry, field) {
					return fmt.Errorf("baseline port-map entry %s %s gate requires nonempty %s", entry.ID, stageName, field)
				}
			}
		}
		seenEntryCells := make(map[string]bool, len(entry.AcceptanceCells))
		for _, cellID := range entry.AcceptanceCells {
			if !knownCells[cellID] {
				return fmt.Errorf("baseline port-map entry %s references unknown acceptance cell %s", entry.ID, cellID)
			}
			if seenEntryCells[cellID] {
				return fmt.Errorf("baseline port-map entry %s repeats acceptance cell %s", entry.ID, cellID)
			}
			seenEntryCells[cellID] = true
			coveredCells[cellID] = true
		}
	}
	if len(portMap.Entries) == 0 {
		return errors.New("baseline port map has no entries")
	}
	if portMap.TraceabilityGate.RequireCompleteAcceptanceUnion {
		missing := make([]string, 0)
		for id := range knownCells {
			if !coveredCells[id] {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		if len(missing) != 0 {
			return fmt.Errorf("baseline port-map acceptance union is incomplete; missing %s", strings.Join(missing, ", "))
		}
	}

	if previous == nil {
		return nil
	}
	if previous.ManifestRevision <= 0 || portMap.ManifestRevision <= previous.ManifestRevision {
		return fmt.Errorf("baseline port-map manifest revision %d must be greater than prior revision %d", portMap.ManifestRevision, previous.ManifestRevision)
	}
	previousStatusIndex := orderedIndex(previous.StatusOrder)
	for _, priorEntry := range previous.Entries {
		current, ok := entries[priorEntry.ID]
		if !ok {
			return fmt.Errorf("baseline port-map entry %s disappeared across manifest revisions", priorEntry.ID)
		}
		priorStage, priorKnown := previousStatusIndex[priorEntry.Status]
		currentStage, currentKnown := statusIndex[current.Status]
		if !priorKnown || !currentKnown {
			return fmt.Errorf("baseline port-map entry %s has an unknown status across manifest revisions", priorEntry.ID)
		}
		if currentStage < priorStage {
			return fmt.Errorf("baseline port-map entry %s status regressed from %s to %s", priorEntry.ID, priorEntry.Status, current.Status)
		}
	}
	return nil
}

// RequireImplementation rejects production work before mapped baseline evidence exists.
func (portMap *BaselinePortMap) RequireImplementation(ids ...string) error {
	return portMap.requireGate("implementation", portMap.ImplementationGate, ids)
}

// RequireDeletion rejects legacy deletion before real installed evidence permits it.
func (portMap *BaselinePortMap) RequireDeletion(ids ...string) error {
	return portMap.requireGate("deletion", portMap.DeletionGate, ids)
}

//nolint:gocyclo // Contract validation keeps all cumulative fail-closed stage gates explicit.
func (portMap *BaselinePortMap) validateContract() error {
	if portMap == nil || portMap.SchemaVersion != 1 || portMap.ManifestRevision <= 0 ||
		strings.TrimSpace(portMap.BaselineCommit) == "" || strings.TrimSpace(portMap.AcceptanceManifest) == "" {
		return errors.New("baseline port-map schema identity is incomplete")
	}
	if len(portMap.StatusOrder) == 0 {
		return errors.New("baseline port-map status order is empty")
	}
	statusIndex := orderedIndex(portMap.StatusOrder)
	if len(statusIndex) != len(portMap.StatusOrder) {
		return errors.New("baseline port-map status order contains empty or duplicate statuses")
	}
	if portMap.StatusContract.Storage != "current-status-only" ||
		!portMap.StatusContract.MonotonicAcrossManifestRevisions ||
		portMap.StatusContract.Validation != "cumulative-stage-predicates" ||
		portMap.StatusContract.EvidenceField != "evidence" ||
		!portMap.StatusContract.CurrentStatusMustSatisfyAllApplicableGates ||
		portMap.StatusContract.ScalarStatusHistoryRequired {
		return errors.New("baseline port-map current-status contract is invalid")
	}
	if len(portMap.StatusContract.StageGates) != len(portMap.StatusOrder) {
		return errors.New("baseline port-map stage gates do not match status order")
	}
	for _, status := range portMap.StatusOrder {
		gate, ok := portMap.StatusContract.StageGates[status]
		if !ok || len(gate.RequiredNonemptyFields) == 0 {
			return fmt.Errorf("baseline port-map stage %s has no required fields", status)
		}
		for _, field := range gate.RequiredNonemptyFields {
			if !knownPortEntryField(field) {
				return fmt.Errorf("baseline port-map stage %s references unknown entry field %s", status, field)
			}
		}
	}
	if !portMap.TraceabilityGate.RequireAcceptanceManifestReferences || !portMap.TraceabilityGate.RequireCompleteAcceptanceUnion {
		return errors.New("baseline port-map traceability gate is incomplete")
	}
	if portMap.ImplementationGate.Scope != "targeted_entries" {
		return errors.New("baseline port-map implementation gate scope is invalid")
	}
	for name, gate := range map[string]PortMapGate{"implementation": portMap.ImplementationGate, "deletion": portMap.DeletionGate} {
		if _, ok := statusIndex[gate.MinimumStatus]; !ok || len(gate.RequiredNonemptyFields) == 0 {
			return fmt.Errorf("baseline port-map %s gate is incomplete", name)
		}
		for _, field := range gate.RequiredNonemptyFields {
			if !knownPortEntryField(field) {
				return fmt.Errorf("baseline port-map %s gate references unknown entry field %s", name, field)
			}
		}
	}
	return nil
}

func (portMap *BaselinePortMap) requireGate(name string, gate PortMapGate, ids []string) error {
	if err := portMap.validateContract(); err != nil {
		return err
	}
	statusIndex := orderedIndex(portMap.StatusOrder)
	minimum := statusIndex[gate.MinimumStatus]
	entries := make(map[string]BaselinePortEntry, len(portMap.Entries))
	for _, entry := range portMap.Entries {
		entries[entry.ID] = entry
	}
	for _, id := range ids {
		entry, ok := entries[id]
		if !ok {
			return fmt.Errorf("unknown port-map entry %s", id)
		}
		stage, ok := statusIndex[entry.Status]
		if !ok || stage < minimum {
			return fmt.Errorf("%s gate for %s requires status %s", name, id, gate.MinimumStatus)
		}
		for _, field := range gate.RequiredNonemptyFields {
			if !portEntryFieldNonempty(entry, field) {
				return fmt.Errorf("%s gate for %s requires nonempty %s", name, id, field)
			}
		}
	}
	return nil
}

func orderedIndex(values []string) map[string]int {
	result := make(map[string]int, len(values))
	for index, value := range values {
		if strings.TrimSpace(value) != "" {
			result[value] = index
		}
	}
	return result
}

func knownPortEntryField(field string) bool {
	switch field {
	case "id", "product", "baseline_paths", "old_symbols", "old_tests", "invariant", "new_owner",
		"new_symbols", "replacement_tests", "acceptance_cells", "evidence", "deletion_paths", "status":
		return true
	default:
		return false
	}
}

func portEntryFieldNonempty(entry BaselinePortEntry, field string) bool {
	switch field {
	case "id":
		return strings.TrimSpace(entry.ID) != ""
	case "product":
		return strings.TrimSpace(entry.Product) != ""
	case "baseline_paths":
		return stringSliceNonempty(entry.BaselinePaths)
	case "old_symbols":
		return stringSliceNonempty(entry.OldSymbols)
	case "old_tests":
		return stringSliceNonempty(entry.OldTests)
	case "invariant":
		return strings.TrimSpace(entry.Invariant) != ""
	case "new_owner":
		return strings.TrimSpace(entry.NewOwner) != ""
	case "new_symbols":
		return stringSliceNonempty(entry.NewSymbols)
	case "replacement_tests":
		return stringSliceNonempty(entry.ReplacementTests)
	case "acceptance_cells":
		return stringSliceNonempty(entry.AcceptanceCells)
	case "evidence":
		return stringSliceNonempty(entry.Evidence)
	case "deletion_paths":
		return stringSliceNonempty(entry.DeletionPaths)
	case "status":
		return strings.TrimSpace(entry.Status) != ""
	default:
		return false
	}
}

func stringSliceNonempty(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}
