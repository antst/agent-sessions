package daemon

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// MigrationSchemaVersion is the only migration journal schema understood by
// this daemon generation.
const MigrationSchemaVersion = 1

// MigrationState is the durable phase of the one-time legacy convergence.
type MigrationState string

//nolint:revive // Constant names retain the complete durable state prefix.
const (
	MigrationStateInventorying            MigrationState = "inventorying"
	MigrationStateBlockedActivePeerOrLane MigrationState = "blocked_active_peer_or_lane"
	MigrationStateBlockedLiveAuthority    MigrationState = "blocked_live_legacy_authority"
	MigrationStateBlockedUnknownIdentity  MigrationState = "blocked_unknown_identity"
	MigrationStateLegacyAbsenceVerified   MigrationState = "legacy_absence_verified"
	MigrationStateAdopting                MigrationState = "adopting"
	MigrationStateAuthorityCommitted      MigrationState = "authority_committed"
	MigrationStateRetiringLegacyArtifacts MigrationState = "retiring_legacy_artifacts"
	MigrationStateComplete                MigrationState = "complete"
	MigrationStateDebt                    MigrationState = "debt"
	MigrationStateRetryRequired           MigrationState = "retry_required"
)

// MaintenanceWindowState records only evidence established while the
// operator-owned no-launch maintenance window is held.
type MaintenanceWindowState string

const (
	// MaintenanceWindowUnverified means exact legacy absence is not established.
	MaintenanceWindowUnverified MaintenanceWindowState = "unverified"
	// MaintenanceWindowBlocked means named live or ambiguous legacy evidence blocks admission.
	MaintenanceWindowBlocked MaintenanceWindowState = "blocked"
	// MaintenanceWindowLegacyAbsenceVerified means every exact authority is absent.
	MaintenanceWindowLegacyAbsenceVerified MaintenanceWindowState = "legacy_absence_verified"
)

//nolint:revive // Constant names retain the complete legacy classification prefix.
const (
	LegacyClassificationActiveManagedBlocker = "active_managed_blocker"
	LegacyClassificationLiveLegacyAuthority  = "live_legacy_authority"
	LegacyClassificationStale                = "stale"
	LegacyClassificationConflicting          = "conflicting"
	LegacyClassificationUnknown              = "unknown"
	LegacyClassificationRetired              = "retired_artifact"
	LegacyClassificationExcluded             = "excluded"
)

// MigrationTransaction is the revisioned durable journal for one legacy
// convergence. Candidate and debt bodies live in their own bounded records;
// this journal contains their exact durable identifiers.
type MigrationTransaction struct {
	SchemaVersion             int                      `json:"schema_version"`
	MigrationID               string                   `json:"migration_id"`
	FromVersions              []string                 `json:"from_versions"`
	TargetRuntimeIdentity     string                   `json:"target_runtime_identity"`
	State                     MigrationState           `json:"state"`
	Candidates                []string                 `json:"candidates"`
	AdoptedRecords            []MigrationProvenance    `json:"adopted_records,omitempty"`
	ActiveManagedBlockers     []LegacyMigrationBlocker `json:"active_managed_blockers,omitempty"`
	LiveAuthorityBlockers     []LegacyMigrationBlocker `json:"live_legacy_authority_blockers,omitempty"`
	CleanupDebtIDs            []string                 `json:"cleanup_debt_ids,omitempty"`
	AuthorityGeneration       uint64                   `json:"authority_generation,omitempty"`
	SuccessorStateDurable     bool                     `json:"successor_state_durable"`
	MaintenanceWindowState    MaintenanceWindowState   `json:"maintenance_window_state"`
	VerifiedAbsentAuthorities []string                 `json:"verified_absent_authority_ids,omitempty"`
	RetiredCandidateIDs       []string                 `json:"retired_candidate_ids,omitempty"`
	PriorAuthority            *LegacyPriorAuthority    `json:"prior_authority,omitempty"`
	RollbackCompleted         bool                     `json:"rollback_completed,omitempty"`
	FreshInventoryRequired    bool                     `json:"fresh_inventory_required,omitempty"`
	RollbackCause             string                   `json:"rollback_cause,omitempty"`
	Revision                  uint64                   `json:"revision"`
	StartedAt                 int64                    `json:"started_at"`
	UpdatedAt                 int64                    `json:"updated_at"`
	CompletedAt               int64                    `json:"completed_at,omitempty"`
}

// Validate rejects incomplete journal authority, unsupported transitions, and
// an authority commit that lacks the two mandatory migration gates.
//
//nolint:gocyclo // Each journal reference and authority gate fails independently for actionable diagnostics.
func (record MigrationTransaction) Validate() error {
	if record.SchemaVersion != MigrationSchemaVersion || record.Revision == 0 {
		return errors.New("migration transaction has an unsupported schema or zero revision")
	}
	if !durableRecordID.MatchString(record.MigrationID) || strings.TrimSpace(record.TargetRuntimeIdentity) == "" {
		return errors.New("migration transaction has incomplete exact identity")
	}
	if record.StartedAt <= 0 || record.UpdatedAt < record.StartedAt || record.CompletedAt < 0 ||
		(record.CompletedAt > 0 && record.UpdatedAt < record.CompletedAt) {
		return errors.New("migration transaction has invalid timestamps")
	}
	if err := validateMigrationStrings("from version", record.FromVersions, true); err != nil {
		return err
	}
	if err := validateMigrationIDs("candidate", record.Candidates); err != nil {
		return err
	}
	if err := validateMigrationIDs("cleanup debt", record.CleanupDebtIDs); err != nil {
		return err
	}
	if err := validateMigrationIDs("verified-absent authority", record.VerifiedAbsentAuthorities); err != nil {
		return err
	}
	if err := validateMigrationIDs("retired candidate", record.RetiredCandidateIDs); err != nil {
		return err
	}
	if record.PriorAuthority != nil {
		if err := record.PriorAuthority.Validate(); err != nil {
			return fmt.Errorf("migration transaction prior authority: %w", err)
		}
	}
	if record.RollbackCompleted && strings.TrimSpace(record.RollbackCause) == "" {
		return errors.New("completed migration rollback lacks a bounded cause")
	}
	if record.FreshInventoryRequired && (record.State != MigrationStateRetryRequired || record.SuccessorStateDurable ||
		record.MaintenanceWindowState != MaintenanceWindowUnverified || record.AuthorityGeneration != 0 ||
		len(record.VerifiedAbsentAuthorities) != 0 || len(record.RetiredCandidateIDs) != 0) {
		return errors.New("fresh-inventory migration boundary retains stale authority")
	}
	blockerIDs := make([]string, 0, len(record.ActiveManagedBlockers))
	for _, blocker := range record.ActiveManagedBlockers {
		if err := blocker.Validate(); err != nil {
			return fmt.Errorf("migration transaction blocker: %w", err)
		}
		if len(record.Candidates) > 0 && !slices.Contains(record.Candidates, blocker.CandidateID) {
			return fmt.Errorf("migration blocker references unknown candidate %q", blocker.CandidateID)
		}
		blockerIDs = append(blockerIDs, blocker.BlockerID)
	}
	if err := validateMigrationIDs("blocker", blockerIDs); err != nil {
		return err
	}
	liveBlockerIDs := make([]string, 0, len(record.LiveAuthorityBlockers))
	for _, blocker := range record.LiveAuthorityBlockers {
		if err := blocker.Validate(); err != nil {
			return fmt.Errorf("migration transaction live-authority blocker: %w", err)
		}
		if blocker.ResourceType != "authority" {
			return errors.New("live legacy authority blocker is not authority-scoped")
		}
		if !slices.Contains(record.Candidates, blocker.CandidateID) {
			return fmt.Errorf("live legacy authority blocker references unknown candidate %q", blocker.CandidateID)
		}
		liveBlockerIDs = append(liveBlockerIDs, blocker.BlockerID)
	}
	if err := validateMigrationIDs("live-authority blocker", liveBlockerIDs); err != nil {
		return err
	}
	provenanceIDs := make([]string, 0, len(record.AdoptedRecords))
	for _, provenance := range record.AdoptedRecords {
		if err := provenance.Validate(); err != nil {
			return fmt.Errorf("migration transaction provenance: %w", err)
		}
		if len(record.Candidates) > 0 && !slices.Contains(record.Candidates, provenance.CandidateID) {
			return fmt.Errorf("migration provenance references unknown candidate %q", provenance.CandidateID)
		}
		provenanceIDs = append(provenanceIDs, provenance.ProvenanceID)
	}
	if err := validateMigrationIDs("provenance", provenanceIDs); err != nil {
		return err
	}
	if !validMigrationState(record.State) {
		if record.State == "ready_to_stop_legacy" || record.State == "stopping_legacy" {
			return fmt.Errorf("superseded migration state %q requires operator review and fresh inventory debt", record.State)
		}
		return fmt.Errorf("unsupported migration state %q", record.State)
	}
	if record.State == MigrationStateBlockedActivePeerOrLane && len(record.ActiveManagedBlockers) == 0 {
		return errors.New("active-peer-or-lane migration state has no exact blockers")
	}
	if record.State != MigrationStateBlockedActivePeerOrLane && len(record.ActiveManagedBlockers) != 0 {
		return errors.New("migration transaction retains active blockers outside its blocked state")
	}
	if record.State == MigrationStateBlockedLiveAuthority && len(record.LiveAuthorityBlockers) == 0 {
		return errors.New("blocked-live-authority migration state has no exact authority blockers")
	}
	if record.State != MigrationStateBlockedLiveAuthority && len(record.LiveAuthorityBlockers) != 0 {
		return errors.New("migration transaction retains live-authority blockers outside its blocked state")
	}
	if (record.State == MigrationStateBlockedUnknownIdentity || record.State == MigrationStateDebt) &&
		len(record.CleanupDebtIDs) == 0 {
		return errors.New("blocked or debt migration state has no exact cleanup debt")
	}
	if err := validateMaintenanceWindowState(record); err != nil {
		return err
	}
	if migrationStateHasCommittedAuthority(record.State) &&
		(!record.SuccessorStateDurable ||
			record.MaintenanceWindowState != MaintenanceWindowLegacyAbsenceVerified || record.AuthorityGeneration == 0) {
		return errors.New("migration authority cannot commit before durable successor state and verified legacy absence")
	}
	if record.State == MigrationStateComplete {
		if record.CompletedAt == 0 {
			return errors.New("complete migration transaction lacks completed_at")
		}
	} else if record.CompletedAt != 0 {
		return errors.New("non-complete migration transaction has completed_at")
	}
	return nil
}

// Advance returns the next durable transaction revision. It never mutates the
// receiver and accepts only the state graph defined by the migration contract.
func (record MigrationTransaction) Advance(next MigrationState, observedAt int64) (MigrationTransaction, error) {
	if !validMigrationTransition(record.State, next) {
		return MigrationTransaction{}, fmt.Errorf("unsupported migration transition %q -> %q", record.State, next)
	}
	if observedAt <= record.UpdatedAt {
		return MigrationTransaction{}, errors.New("migration transition timestamp must advance")
	}
	nextRecord := record.clone()
	nextRecord.State = next
	nextRecord.Revision++
	nextRecord.UpdatedAt = observedAt
	if next == MigrationStateComplete {
		nextRecord.CompletedAt = observedAt
	} else {
		nextRecord.CompletedAt = 0
	}
	if err := nextRecord.Validate(); err != nil {
		return MigrationTransaction{}, err
	}
	return nextRecord, nil
}

// LegacyInventoryOptions names the bounded platform-specific roots that T080
// will project into a closed inventory. It intentionally has no scan fallback.
type LegacyInventoryOptions struct {
	Platform             string   `json:"platform"`
	UID                  int      `json:"uid"`
	HomeDir              string   `json:"home_dir"`
	StateHome            string   `json:"state_home"`
	RuntimeDir           string   `json:"runtime_dir,omitempty"`
	SystemTempDir        string   `json:"system_temp_dir"`
	RecordedRuntimeRoots []string `json:"recorded_runtime_roots,omitempty"`
}

// LegacyInventorySource is one exact, bounded source in the closed inventory.
type LegacyInventorySource struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Target   bool   `json:"target"`
	MaxDepth int    `json:"max_depth"`
}

// LegacyProcessObservation is exact process evidence captured during an
// inventory revision.
type LegacyProcessObservation struct {
	Status      string `json:"status"`
	PID         int    `json:"pid,omitempty"`
	ProcStart   string `json:"proc_start,omitempty"`
	StrongStart string `json:"strong_start,omitempty"`
	Executable  string `json:"executable,omitempty"`
	ArgvRole    string `json:"argv_role,omitempty"`
}

// LegacyEndpointObservation is kernel-attributed local endpoint evidence.
type LegacyEndpointObservation struct {
	Status          string `json:"status"`
	Path            string `json:"path,omitempty"`
	Type            string `json:"type,omitempty"`
	OwnerUID        int    `json:"owner_uid,omitempty"`
	OwnerPID        int    `json:"owner_pid,omitempty"`
	OwnerProcStart  string `json:"owner_proc_start,omitempty"`
	RuntimeIdentity string `json:"runtime_identity,omitempty"`
}

// LegacyServiceObservation is one exact supported user-service identity.
type LegacyServiceObservation struct {
	Status     string `json:"status"`
	Manager    string `json:"manager,omitempty"`
	Unit       string `json:"unit,omitempty"`
	Executable string `json:"executable,omitempty"`
	ArgvRole   string `json:"argv_role,omitempty"`
}

// LegacyCandidateEvidence is the immutable observation input used by T080's
// classifier. Scalar counts are retained only as evidence and never authority.
type LegacyCandidateEvidence struct {
	CandidateID         string                    `json:"candidate_id"`
	Kind                string                    `json:"kind"`
	SourcePath          string                    `json:"source_path"`
	SourceRevision      string                    `json:"source_revision,omitempty"`
	ArtifactRevision    string                    `json:"artifact_revision,omitempty"`
	ArtifactIdentity    string                    `json:"artifact_identity,omitempty"`
	ReportedVersion     string                    `json:"reported_version,omitempty"`
	RuntimeIdentity     string                    `json:"runtime_identity,omitempty"`
	PID                 int                       `json:"pid,omitempty"`
	ProcStart           string                    `json:"proc_start,omitempty"`
	StrongStart         string                    `json:"strong_start,omitempty"`
	Process             LegacyProcessObservation  `json:"process"`
	Endpoint            LegacyEndpointObservation `json:"endpoint"`
	Service             LegacyServiceObservation  `json:"service"`
	RelatedSessionIDs   []string                  `json:"related_session_ids,omitempty"`
	RelatedLaneIDs      []string                  `json:"related_lane_ids,omitempty"`
	ReportedActiveCount int                       `json:"reported_active_count,omitempty"`
	EvidenceRevision    uint64                    `json:"evidence_revision,omitempty"`
	ObservedAt          int64                     `json:"observed_at,omitempty"`
}

// LegacyRuntimeCandidate is the revisioned classification of one legacy
// Agent Sessions process, endpoint, service, or state owner.
type LegacyRuntimeCandidate struct {
	SchemaVersion           int               `json:"schema_version"`
	CandidateID             string            `json:"candidate_id"`
	Kind                    string            `json:"kind"`
	SourcePath              string            `json:"source_path"`
	SourceRevision          string            `json:"source_revision,omitempty"`
	ArtifactRevision        string            `json:"artifact_revision,omitempty"`
	ArtifactIdentity        string            `json:"artifact_identity,omitempty"`
	ReportedVersion         string            `json:"reported_version,omitempty"`
	RuntimeIdentity         string            `json:"runtime_identity,omitempty"`
	PID                     int               `json:"pid,omitempty"`
	ProcStart               string            `json:"proc_start,omitempty"`
	StrongStart             string            `json:"strong_start,omitempty"`
	ProcessStatus           string            `json:"process_status,omitempty"`
	EndpointPath            string            `json:"endpoint_identity,omitempty"`
	EndpointStatus          string            `json:"endpoint_status,omitempty"`
	EndpointType            string            `json:"endpoint_type,omitempty"`
	EndpointOwnerUID        int               `json:"endpoint_owner_uid,omitempty"`
	EndpointOwnerPID        int               `json:"endpoint_owner_pid,omitempty"`
	EndpointOwnerStart      string            `json:"endpoint_owner_proc_start,omitempty"`
	EndpointRuntimeIdentity string            `json:"endpoint_runtime_identity,omitempty"`
	ServiceManager          string            `json:"service_manager,omitempty"`
	ServiceUnit             string            `json:"service_identity,omitempty"`
	ServiceStatus           string            `json:"service_status,omitempty"`
	ServiceExecutable       string            `json:"service_executable,omitempty"`
	ServiceArgvRole         string            `json:"service_argv_role,omitempty"`
	ProcessExecutable       string            `json:"process_executable,omitempty"`
	ProcessArgvRole         string            `json:"process_argv_role,omitempty"`
	ProcessArguments        []string          `json:"process_arguments,omitempty"`
	ProcessEnvironment      map[string]string `json:"process_environment,omitempty"`
	RelatedSessionIDs       []string          `json:"related_session_ids,omitempty"`
	RelatedLaneIDs          []string          `json:"related_lane_ids,omitempty"`
	ReportedActiveCount     int               `json:"reported_active_count,omitempty"`
	Classification          string            `json:"classification"`
	EvidenceRevision        uint64            `json:"evidence_revision"`
	LastObservedAt          int64             `json:"last_observed_at"`
}

// Validate checks the durable candidate envelope. Classification behavior and
// evidence reconciliation belong to T080 rather than this record definition.
func (record LegacyRuntimeCandidate) Validate() error {
	if record.SchemaVersion != MigrationSchemaVersion || record.EvidenceRevision == 0 || record.LastObservedAt <= 0 {
		return errors.New("legacy runtime candidate has an unsupported schema or zero evidence revision")
	}
	if !durableRecordID.MatchString(record.CandidateID) || strings.TrimSpace(record.Kind) == "" ||
		!migrationAbsoluteCleanPath(record.SourcePath) {
		return errors.New("legacy runtime candidate has incomplete exact source identity")
	}
	if record.ArtifactRevision != "" && !validLegacyArtifactDigest(record.ArtifactRevision) {
		return errors.New("legacy runtime candidate has an invalid exact artifact revision")
	}
	if record.ArtifactIdentity != "" && !validLegacyArtifactDigest(record.ArtifactIdentity) {
		return errors.New("legacy runtime candidate has an invalid exact artifact identity")
	}
	if !validLegacyClassification(record.Classification) {
		return fmt.Errorf("unsupported legacy candidate classification %q", record.Classification)
	}
	if err := validateLegacyCandidateRelations(record); err != nil {
		return err
	}
	return validateLegacyCandidateProcessContract(record)
}

func validLegacyArtifactDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && len(value) == len("sha256:")+64
}

func validateLegacyCandidateRelations(record LegacyRuntimeCandidate) error {
	if err := validateMigrationIDs("related session", record.RelatedSessionIDs); err != nil {
		return err
	}
	if err := validateMigrationIDs("related lane", record.RelatedLaneIDs); err != nil {
		return err
	}
	return nil
}

func validateLegacyCandidateProcessContract(record LegacyRuntimeCandidate) error {
	if record.EndpointPath != "" && !migrationAbsoluteCleanPath(record.EndpointPath) {
		return errors.New("legacy runtime candidate has a noncanonical endpoint identity")
	}
	if (record.ServiceManager == "") != (record.ServiceUnit == "") {
		return errors.New("legacy runtime candidate has incomplete service identity")
	}
	if len(record.ProcessArguments) > 32 {
		return errors.New("legacy runtime candidate has an unbounded process argument contract")
	}
	for _, argument := range record.ProcessArguments {
		if len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return errors.New("legacy runtime candidate has an invalid process argument contract")
		}
	}
	if len(record.ProcessEnvironment) != 0 {
		if record.Kind != "supervisor" {
			return errors.New("legacy runtime candidate has an unsupported process environment contract")
		}
		if err := validateLegacySupervisorEvidenceEnvironment(record.ProcessEnvironment); err != nil {
			return err
		}
	}
	return nil
}

func validateLegacySupervisorEvidenceEnvironment(environment map[string]string) error {
	allowed := make(map[string]struct{})
	for _, name := range legacySupervisorEnvironmentNames() {
		allowed[name] = struct{}{}
	}
	if len(environment) > len(allowed) {
		return errors.New("legacy supervisor evidence environment exceeds its closed allowlist")
	}
	for name, value := range environment {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("legacy supervisor evidence environment contains unsupported variable %q", name)
		}
		if name == "CLAUDE_SECURESTORAGE_CONFIG_DIR" && value == "" {
			continue
		}
		if !migrationAbsoluteCleanPath(value) {
			return fmt.Errorf("legacy supervisor evidence environment variable %q is noncanonical", name)
		}
	}
	for _, name := range legacySupervisorRequiredEnvironmentNames() {
		if _, ok := environment[name]; !ok {
			return fmt.Errorf("legacy supervisor evidence environment lacks required variable %q", name)
		}
	}
	return nil
}

func legacySupervisorRequiredEnvironmentNames() []string {
	return []string{
		"HOME",
		"CODEX_HOME",
		"CLAUDE_PEER_DATA_DIR",
		"CLAUDE_PEER_SUPERVISOR_SOCKET",
		"CLAUDE_PEER_APP_SERVER_SOCKET",
	}
}

func legacySupervisorEnvironmentNames() []string {
	return []string{
		"HOME",
		"CODEX_HOME",
		"CLAUDE_PEER_DATA_DIR",
		"CLAUDE_PEER_SUPERVISOR_SOCKET",
		"CLAUDE_PEER_APP_SERVER_SOCKET",
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR",
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR",
		"XDG_RUNTIME_DIR",
		"TMPDIR",
	}
}

// LegacyMigrationBlocker names one exact peer or lane that must be closed by
// the operator before migration is retried.
type LegacyMigrationBlocker struct {
	SchemaVersion    int    `json:"schema_version,omitempty"`
	Revision         uint64 `json:"revision,omitempty"`
	BlockerID        string `json:"blocker_id,omitempty"`
	CandidateID      string `json:"candidate_id"`
	Kind             string `json:"kind"`
	ResourceType     string `json:"resource_type"`
	ResourceID       string `json:"resource_id"`
	RequiredAction   string `json:"required_action"`
	EvidenceRevision uint64 `json:"evidence_revision,omitempty"`
	LastObservedAt   int64  `json:"last_observed_at,omitempty"`
}

// Validate checks revisioned, exact, actionable blocker identity.
func (record LegacyMigrationBlocker) Validate() error {
	if record.SchemaVersion != MigrationSchemaVersion || record.Revision == 0 || record.EvidenceRevision == 0 ||
		record.LastObservedAt <= 0 {
		return errors.New("legacy migration blocker has an unsupported schema or zero revision")
	}
	if !durableRecordID.MatchString(record.BlockerID) || !durableRecordID.MatchString(record.CandidateID) ||
		strings.TrimSpace(record.Kind) == "" ||
		(record.ResourceType != "peer" && record.ResourceType != "lane" && record.ResourceType != "authority") ||
		!durableRecordID.MatchString(record.ResourceID) || record.RequiredAction != "close_before_retry" {
		return errors.New("legacy migration blocker has incomplete exact actionable identity")
	}
	return nil
}

// MigrationProvenance binds one exact legacy source revision to one adopted
// daemon-owned record revision without copying vendor-owned data.
type MigrationProvenance struct {
	SchemaVersion  int    `json:"schema_version"`
	Revision       uint64 `json:"revision"`
	ProvenanceID   string `json:"provenance_id"`
	CandidateID    string `json:"candidate_id"`
	SourcePath     string `json:"source_path"`
	SourceRevision string `json:"source_revision"`
	TargetKind     string `json:"target_kind"`
	TargetRecordID string `json:"target_record_id"`
	TargetRevision uint64 `json:"target_revision"`
	AdoptedAt      int64  `json:"adopted_at"`
}

// Validate checks both sides of one immutable adoption assertion.
func (record MigrationProvenance) Validate() error {
	if record.SchemaVersion != MigrationSchemaVersion || record.Revision == 0 || record.TargetRevision == 0 ||
		record.AdoptedAt <= 0 {
		return errors.New("migration provenance has an unsupported schema or zero revision")
	}
	if !durableRecordID.MatchString(record.ProvenanceID) || !durableRecordID.MatchString(record.CandidateID) ||
		!migrationAbsoluteCleanPath(record.SourcePath) || strings.TrimSpace(record.SourceRevision) == "" ||
		strings.TrimSpace(record.TargetKind) == "" || !durableRecordID.MatchString(record.TargetRecordID) {
		return errors.New("migration provenance has incomplete exact source or target identity")
	}
	return nil
}

// LegacyMigrationDebt is bounded retryable proof that exact legacy identity
// cannot yet be classified or retired safely.
type LegacyMigrationDebt struct {
	SchemaVersion    int    `json:"schema_version,omitempty"`
	Revision         uint64 `json:"revision,omitempty"`
	DebtID           string `json:"debt_id"`
	CandidateID      string `json:"candidate_id"`
	Code             string `json:"code"`
	Retryable        bool   `json:"retryable"`
	ExpectedIdentity string `json:"expected_identity,omitempty"`
	ObservedIdentity string `json:"observed_identity,omitempty"`
	RetryPredicate   string `json:"retry_predicate,omitempty"`
	ProhibitedScope  string `json:"prohibited_scope,omitempty"`
	EvidenceRevision uint64 `json:"evidence_revision,omitempty"`
	CreatedAt        int64  `json:"created_at,omitempty"`
	UpdatedAt        int64  `json:"updated_at,omitempty"`
	ResolvedAt       int64  `json:"resolved_at,omitempty"`
}

// Validate checks durable debt identity and the exact retry/prohibition rule.
func (record LegacyMigrationDebt) Validate() error {
	if record.SchemaVersion != MigrationSchemaVersion || record.Revision == 0 {
		return errors.New("legacy migration debt has an unsupported schema or zero revision")
	}
	if !durableRecordID.MatchString(record.DebtID) || !durableRecordID.MatchString(record.CandidateID) ||
		strings.TrimSpace(record.Code) == "" || !record.Retryable {
		return errors.New("legacy migration debt has incomplete retryable identity")
	}
	if strings.TrimSpace(record.RetryPredicate) == "" ||
		strings.TrimSpace(record.ProhibitedScope) == "" || record.EvidenceRevision == 0 ||
		record.CreatedAt <= 0 || record.UpdatedAt < record.CreatedAt || record.ResolvedAt < 0 ||
		(record.ResolvedAt > 0 && record.UpdatedAt < record.ResolvedAt) {
		return errors.New("durable legacy migration debt has invalid evidence, retry policy, or timestamps")
	}
	return nil
}

// LegacyQuiescenceRequest and LegacyQuiescenceReport are the typed boundary
// for T081's read-only global gate.
type LegacyQuiescenceRequest struct {
	Candidates []LegacyRuntimeCandidate `json:"candidates"`
}

// LegacyQuiescenceReport is the content-free result of the read-only global gate.
type LegacyQuiescenceReport struct {
	LegacyAbsenceVerified bool                     `json:"legacy_absence_verified"`
	Strategy              string                   `json:"strategy"`
	Blockers              []LegacyMigrationBlocker `json:"blockers,omitempty"`
	Debt                  []LegacyMigrationDebt    `json:"debt,omitempty"`
	StaleCandidateIDs     []string                 `json:"stale_candidate_ids,omitempty"`
	RetryInstruction      string                   `json:"retry_instruction,omitempty"`
}

// ErrLegacyQuiescenceBlocked identifies the safe, non-mutating refusal class.
var ErrLegacyQuiescenceBlocked = errors.New("legacy migration is blocked")

func (record MigrationTransaction) clone() MigrationTransaction {
	record.FromVersions = slices.Clone(record.FromVersions)
	record.Candidates = slices.Clone(record.Candidates)
	record.AdoptedRecords = slices.Clone(record.AdoptedRecords)
	record.ActiveManagedBlockers = slices.Clone(record.ActiveManagedBlockers)
	record.CleanupDebtIDs = slices.Clone(record.CleanupDebtIDs)
	record.LiveAuthorityBlockers = slices.Clone(record.LiveAuthorityBlockers)
	record.VerifiedAbsentAuthorities = slices.Clone(record.VerifiedAbsentAuthorities)
	record.RetiredCandidateIDs = slices.Clone(record.RetiredCandidateIDs)
	if record.PriorAuthority != nil {
		prior := *record.PriorAuthority
		prior.Candidate.ProcessArguments = slices.Clone(prior.Candidate.ProcessArguments)
		prior.Candidate.ProcessEnvironment = maps.Clone(prior.Candidate.ProcessEnvironment)
		record.PriorAuthority = &prior
	}
	return record
}

func validMigrationState(state MigrationState) bool {
	switch state {
	case MigrationStateInventorying, MigrationStateBlockedActivePeerOrLane,
		MigrationStateBlockedLiveAuthority, MigrationStateBlockedUnknownIdentity,
		MigrationStateLegacyAbsenceVerified, MigrationStateAdopting, MigrationStateAuthorityCommitted,
		MigrationStateRetiringLegacyArtifacts, MigrationStateComplete, MigrationStateDebt,
		MigrationStateRetryRequired:
		return true
	default:
		return false
	}
}

func validateMaintenanceWindowState(record MigrationTransaction) error {
	switch record.MaintenanceWindowState {
	case MaintenanceWindowUnverified, MaintenanceWindowBlocked,
		MaintenanceWindowLegacyAbsenceVerified:
	default:
		return fmt.Errorf("unsupported migration maintenance-window state %q", record.MaintenanceWindowState)
	}

	switch record.State {
	case MigrationStateInventorying, MigrationStateRetryRequired:
		if record.MaintenanceWindowState != MaintenanceWindowUnverified {
			return errors.New("inventory or retry migration claims maintenance-window authority")
		}
	case MigrationStateBlockedActivePeerOrLane, MigrationStateBlockedLiveAuthority,
		MigrationStateBlockedUnknownIdentity:
		if record.MaintenanceWindowState != MaintenanceWindowBlocked {
			return errors.New("blocked migration lacks blocked maintenance-window state")
		}
	case MigrationStateLegacyAbsenceVerified, MigrationStateAdopting,
		MigrationStateAuthorityCommitted, MigrationStateRetiringLegacyArtifacts,
		MigrationStateComplete, MigrationStateDebt:
		if record.MaintenanceWindowState != MaintenanceWindowLegacyAbsenceVerified {
			return errors.New("migration phase lacks verified legacy absence")
		}
	default:
		return fmt.Errorf("unsupported migration state %q", record.State)
	}

	verifiedPhase := record.MaintenanceWindowState == MaintenanceWindowLegacyAbsenceVerified
	if !verifiedPhase {
		if len(record.VerifiedAbsentAuthorities) != 0 {
			return errors.New("unverified maintenance window retains verified-absence authority IDs")
		}
		return nil
	}
	if len(record.Candidates) == 0 || len(record.VerifiedAbsentAuthorities) != len(record.Candidates) {
		return errors.New("verified maintenance window lacks the exact candidate authority set")
	}
	for _, candidateID := range record.Candidates {
		if !slices.Contains(record.VerifiedAbsentAuthorities, candidateID) {
			return fmt.Errorf("verified maintenance window omits candidate authority %q", candidateID)
		}
	}
	return nil
}

func validMigrationTransition(current, next MigrationState) bool {
	allowed := map[MigrationState][]MigrationState{
		MigrationStateInventorying: {
			MigrationStateBlockedActivePeerOrLane, MigrationStateBlockedLiveAuthority,
			MigrationStateBlockedUnknownIdentity, MigrationStateLegacyAbsenceVerified,
		},
		MigrationStateBlockedActivePeerOrLane: {MigrationStateLegacyAbsenceVerified},
		MigrationStateBlockedLiveAuthority:    {MigrationStateLegacyAbsenceVerified},
		MigrationStateBlockedUnknownIdentity:  {MigrationStateLegacyAbsenceVerified, MigrationStateRetryRequired},
		MigrationStateLegacyAbsenceVerified:   {MigrationStateAdopting, MigrationStateRetryRequired},
		MigrationStateAdopting:                {MigrationStateAuthorityCommitted},
		MigrationStateAuthorityCommitted:      {MigrationStateRetiringLegacyArtifacts, MigrationStateDebt},
		MigrationStateRetiringLegacyArtifacts: {MigrationStateComplete, MigrationStateDebt},
		MigrationStateDebt:                    {MigrationStateRetiringLegacyArtifacts, MigrationStateRetryRequired},
	}
	return slices.Contains(allowed[current], next)
}

func migrationStateHasCommittedAuthority(state MigrationState) bool {
	return state == MigrationStateAuthorityCommitted || state == MigrationStateRetiringLegacyArtifacts ||
		state == MigrationStateComplete || state == MigrationStateDebt
}

func validLegacyClassification(classification string) bool {
	switch classification {
	case LegacyClassificationActiveManagedBlocker, LegacyClassificationLiveLegacyAuthority,
		LegacyClassificationStale,
		LegacyClassificationConflicting, LegacyClassificationUnknown,
		LegacyClassificationRetired, LegacyClassificationExcluded:
		return true
	default:
		return false
	}
}

func validateMigrationIDs(kind string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !durableRecordID.MatchString(value) {
			return fmt.Errorf("migration %s has invalid durable identifier %q", kind, value)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("migration %s repeats durable identifier %q", kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateMigrationStrings(kind string, values []string, required bool) error {
	if required && len(values) == 0 {
		return fmt.Errorf("migration %s list is empty", kind)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("migration %s is empty", kind)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("migration %s repeats %q", kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func migrationAbsoluteCleanPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}
