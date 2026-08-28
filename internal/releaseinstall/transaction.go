package releaseinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"

	"golang.org/x/sys/unix"
)

// Phase is one durable install transaction boundary.
type Phase string

const (
	// PhasePrepared records successful role-hook preparation.
	PhasePrepared Phase = "prepared"
	// PhaseStaged records a validated immutable release.
	PhaseStaged Phase = "staged"
	// PhasePointerCommitted records the atomic current-selection switch.
	PhasePointerCommitted Phase = "pointer_committed"
	// PhaseReady records successful exact successor readiness.
	PhaseReady Phase = "ready"
	// PhaseRollForwardRetryable records a failure after the role crossed an
	// irreversible authority boundary. Recovery retries readiness/commit and
	// must never restore the superseded authority from this phase.
	PhaseRollForwardRetryable Phase = "rollforward_retryable"
	// PhaseRollingBack records durable rollback intent before any prior
	// authority or role-owned surface is restored.
	PhaseRollingBack Phase = "rolling_back"
	// PhaseRollbackRetryable records a failed exact rollback step. Recovery
	// resumes rollback and must not attempt candidate startup from this phase.
	PhaseRollbackRetryable Phase = "rollback_retryable"
	// PhaseRolledBack is the terminal state for a failed install whose exact
	// prior authority and role-owned state were restored.
	PhaseRolledBack Phase = "rolled_back"
	// PhaseComplete is the committed terminal transaction state.
	PhaseComplete Phase = "complete"
)

var (
	// ErrInjectedCrash identifies a test-owned crash boundary.
	ErrInjectedCrash = errors.New("injected release transaction crash")
	// ErrRevisionConflict rejects an offline purge plan after state changed.
	ErrRevisionConflict = errors.New("release state revision conflict")
	// ErrRecoveryRequired prevents a new install from overwriting an unfinished
	// transaction whose exact recovery must run first.
	ErrRecoveryRequired = errors.New("release transaction recovery is required")
)

// InstallRequest identifies a complete validated role release source.
type InstallRequest struct {
	Version         string
	ContentIdentity string
	SourceRoot      string
	Executable      string
}

type sourceReleaseExecutable struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	Path string `json:"path"`
}

type sourceReleaseConnector struct {
	Product      string   `json:"product"`
	PluginID     string   `json:"plugin_id"`
	ArchivePaths []string `json:"archive_paths"`
}

type sourceReleaseManifest struct {
	SchemaVersion      int                       `json:"schema_version"`
	ReleaseVersion     string                    `json:"release_version"`
	HubProtocolVersion int                       `json:"hub_protocol_version"`
	Platform           string                    `json:"platform"`
	Checksums          string                    `json:"checksums"`
	Executables        []sourceReleaseExecutable `json:"executables"`
	ConnectorPayloads  []sourceReleaseConnector  `json:"connector_payloads"`
	ServiceAssets      struct {
		Host []string `json:"host"`
		Hub  []string `json:"hub"`
	} `json:"service_assets"`
}

// InstalledRelease is the immutable exact release passed to readiness hooks.
type InstalledRelease struct {
	Role            Role
	ReleaseID       string
	Version         string
	ContentIdentity string
	Root            string
	Executable      string
}

// InstallResult reports the exact selected release and terminal phase.
type InstallResult struct {
	Role      Role
	ReleaseID string
	Phase     Phase
}

// RoleService supplies platform service transitions without introducing a
// second release implementation.
type RoleService interface {
	// Observe reports the exact pre-transaction service enablement and running state.
	Observe(context.Context) (RoleServiceState, error)
	// Reload makes the restored service definition surface authoritative without starting it.
	Reload(context.Context) error
	// Enable restores persistent login activation without starting the service.
	Enable(context.Context) error
	// Restart performs the role descriptor's supported service restart.
	Restart(context.Context) error
	// Stop performs the role descriptor's supported service stop.
	Stop(context.Context) error
	// Disable suppresses login activation for the exact selected role.
	Disable(context.Context) error
	// Start performs the role descriptor's supported service start.
	Start(context.Context) error
	// Verify proves that the exact selected role service is ready.
	Verify(context.Context) error
	// VerifyCandidate proves that the running role service is the exact named immutable release.
	VerifyCandidate(context.Context, InstalledRelease) error
}

// RoleServiceState is the bounded service-manager state captured before an
// install mutates selection, definitions, enablement, or process lifetime.
type RoleServiceState struct {
	Enabled bool
	Running bool
}

// RoleHooks add role-specific preparation, readiness and cleanup around the
// shared transaction mechanics.
type RoleHooks interface {
	// Prepare validates and captures role-specific reversible mutations.
	Prepare(context.Context, InstallRequest) error
	// Ready verifies role-specific exact successor readiness.
	Ready(context.Context, InstalledRelease) error
	// Commit commits prepared role-specific mutations.
	Commit(context.Context) error
	// Rollback restores exact prior role-specific state.
	Rollback(context.Context) error
	// Remove removes only role-specific disposable integrations.
	Remove(context.Context) error
}

// RolePreflight is an optional role hook performed under the cross-process
// install lock, against the original validated source request, and before the
// engine creates a staged release, journal, or prepared role mutation.
// Implementations must be read-only. The host uses this seam for the
// quiescence-only first-migration inspection.
type RolePreflight interface {
	// Preflight performs a read-only exact observation before install mutation.
	Preflight(context.Context, InstallRequest) error
}

// FailureDisposition tells the shared engine whether a role-specific
// readiness failure is still safe to roll back.
type FailureDisposition string

const (
	// FailureDispositionRollback permits the exact pre-ready rollback path.
	FailureDispositionRollback FailureDisposition = "rollback"
	// FailureDispositionRollForward requires retrying the selected successor;
	// the superseded authority has crossed an irreversible commit boundary.
	FailureDispositionRollForward FailureDisposition = "roll_forward"
)

// RoleFailureClassifier is an optional durable role-owned decision queried
// after Ready returns an error. An implementation derives the decision from
// its own journal, never from in-memory progress alone.
type RoleFailureClassifier interface {
	// FailureDisposition classifies the failure at the named release phase.
	FailureDisposition(context.Context, Phase) (FailureDisposition, error)
}

// EngineOptions binds one role layout to its service and hooks.
type EngineOptions struct {
	Layout          RoleLayout
	Service         RoleService
	Hooks           RoleHooks
	PurgeTargets    []string
	PurgeExclusions []string
}

// Engine serializes immutable install, recovery, removal and purge for one role.
type Engine struct {
	layout          RoleLayout
	service         RoleService
	hooks           RoleHooks
	purgeTargets    []string
	purgeExclusions []string
	mu              sync.Mutex
	crashPoint      Phase
}

// TransactionJournal is the durable active role transaction.
type TransactionJournal struct {
	SchemaVersion              int    `json:"schema_version"`
	Role                       Role   `json:"role"`
	Phase                      Phase  `json:"phase"`
	FromRelease                string `json:"from_release,omitempty"`
	ToRelease                  string `json:"to_release"`
	Version                    string `json:"version"`
	ContentIdentity            string `json:"content_identity"`
	Executable                 string `json:"executable"`
	ServiceTransitionAttempted bool   `json:"service_transition_attempted,omitempty"`
	ServiceStateObserved       bool   `json:"service_state_observed"`
	ServiceWasEnabled          bool   `json:"service_was_enabled"`
	ServiceWasRunning          bool   `json:"service_was_running"`
	CandidateStopped           bool   `json:"candidate_stopped,omitempty"`
	CandidateDisabled          bool   `json:"candidate_disabled,omitempty"`
	SelectionRestored          bool   `json:"selection_restored,omitempty"`
	HooksRestored              bool   `json:"hooks_restored,omitempty"`
	ServiceSurfaceReloaded     bool   `json:"service_surface_reloaded,omitempty"`
	PriorServiceRestored       bool   `json:"prior_service_restored,omitempty"`
	ServiceEnablementRestored  bool   `json:"service_enablement_restored,omitempty"`
	FailureCode                string `json:"failure_code,omitempty"`
	RetryCode                  string `json:"retry_code,omitempty"`
	UpdatedAt                  int64  `json:"updated_at"`
}

// PurgePlan identifies exact role-owned targets at one state revision.
type PurgePlan struct {
	Role            Role              `json:"role"`
	Revision        string            `json:"plan_revision"`
	Targets         []string          `json:"targets"`
	TargetRevisions map[string]string `json:"target_revisions"`
	Exclusions      []string          `json:"exclusions"`
}

// NewEngine creates one validated role transaction engine.
func NewEngine(options EngineOptions) (*Engine, error) {
	if options.Layout.Role != RoleHost && options.Layout.Role != RoleHub {
		return nil, errors.New("release engine requires a supported role layout")
	}
	if options.Service == nil || options.Hooks == nil {
		return nil, errors.New("release engine requires service and role hooks")
	}
	want, err := ResolveRoleLayout(options.Layout.Root, options.Layout.Role)
	if err != nil || want != options.Layout {
		return nil, errors.New("release engine layout is not canonical")
	}
	if len(options.PurgeTargets) == 0 {
		options.PurgeTargets = []string{options.Layout.PreservedStateRoot}
	}
	if err := validatePurgeRoots(options.PurgeTargets, options.PurgeExclusions); err != nil {
		return nil, err
	}
	return &Engine{
		layout: options.Layout, service: options.Service, hooks: options.Hooks,
		purgeTargets:    append([]string(nil), options.PurgeTargets...),
		purgeExclusions: append([]string(nil), options.PurgeExclusions...),
	}, nil
}

// SetCrashPoint injects one crash after its durable phase for testing.
func (engine *Engine) SetCrashPoint(phase Phase) { engine.crashPoint = phase }

// Install commits one exact immutable role release or restores the exact prior state.
//
//nolint:gocyclo // Install is the single journaled transaction and keeps every durable phase visible in order.
func (engine *Engine) Install(ctx context.Context, request InstallRequest) (InstallResult, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if request.Executable != expectedRoleExecutable(engine.layout.Role) {
		return InstallResult{}, fmt.Errorf("%s install requires executable %q", engine.layout.Role, expectedRoleExecutable(engine.layout.Role))
	}
	// Reject an indirect or malformed source before creating the role lock or
	// any other install-owned path. Validate again under the install lock below
	// so a source change while waiting cannot gain staging authority.
	if err := validateInstallRequest(request); err != nil {
		return InstallResult{}, err
	}
	unlock, err := acquireInstallLock(engine.layout)
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()

	if err := validateInstallRequest(request); err != nil {
		return InstallResult{}, err
	}
	if err := engine.requireRecoveredJournal(); err != nil {
		return InstallResult{}, err
	}
	if preflight, ok := engine.hooks.(RolePreflight); ok {
		if err := preflight.Preflight(ctx, request); err != nil {
			return InstallResult{}, fmt.Errorf("preflight %s release hooks: %w", engine.layout.Role, err)
		}
	}
	releaseID, _ := ReleaseID(request.Version, request.ContentIdentity)
	prior, err := currentReleaseID(engine.layout)
	if err != nil {
		return InstallResult{}, err
	}
	serviceState, err := engine.service.Observe(ctx)
	if err != nil {
		return InstallResult{}, fmt.Errorf("observe prior %s service state: %w", engine.layout.Role, err)
	}
	journal := TransactionJournal{
		SchemaVersion: 1, Role: engine.layout.Role, Phase: PhaseStaged, FromRelease: prior,
		ToRelease: releaseID, Version: request.Version, ContentIdentity: request.ContentIdentity,
		Executable: request.Executable, UpdatedAt: time.Now().UnixMilli(), ServiceStateObserved: true,
		ServiceWasEnabled: serviceState.Enabled, ServiceWasRunning: serviceState.Running,
	}
	releaseRoot := filepath.Join(engine.layout.ReleasesRoot, releaseID)
	if err := stageImmutableRelease(request, releaseRoot); err != nil {
		return InstallResult{}, fmt.Errorf("stage immutable %s release: %w", engine.layout.Role, err)
	}
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, err
	}
	hookRequest := request
	hookRequest.SourceRoot = releaseRoot
	if err := engine.hooks.Prepare(ctx, hookRequest); err != nil {
		cause := fmt.Errorf("prepare %s release hooks: %w", engine.layout.Role, err)
		return InstallResult{}, engine.rollback(ctx, journal, "role_prepare_failed", cause)
	}
	journal.Phase = PhasePrepared
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, "prepared_journal_failed", err)
	}
	if err := selectRelease(engine.layout, releaseID); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, "selection_failed", err)
	}
	journal.Phase = PhasePointerCommitted
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, "selection_journal_failed", err)
	}
	if engine.crashPoint == PhasePointerCommitted {
		return InstallResult{}, ErrInjectedCrash
	}
	installed := installedFromJournal(engine.layout, journal)
	journal.ServiceTransitionAttempted = true
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, "service_transition_journal_failed", err)
	}
	if err := engine.service.Restart(ctx); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, "candidate_restart_failed", fmt.Errorf("restart candidate service: %w", err))
	}
	if err := engine.hooks.Ready(ctx, installed); err != nil {
		cause := fmt.Errorf("candidate readiness: %w", err)
		return InstallResult{}, engine.handleClassifiedFailure(ctx, journal, "candidate_readiness_failed", cause)
	}
	journal.Phase = PhaseReady
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, engine.handleClassifiedFailure(ctx, journal, "ready_journal_failed", err)
	}
	if engine.crashPoint == PhaseReady {
		return InstallResult{}, ErrInjectedCrash
	}
	if err := engine.hooks.Commit(ctx); err != nil {
		cause := fmt.Errorf("commit role hooks: %w", err)
		return InstallResult{}, engine.handleClassifiedFailure(ctx, journal, "role_commit_failed", cause)
	}
	journal.Phase = PhaseComplete
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Role: engine.layout.Role, ReleaseID: releaseID, Phase: PhaseComplete}, nil
}

func expectedRoleExecutable(role Role) string {
	if role == RoleHub {
		return "agent-sessions-hub"
	}
	return "agent-sessions"
}

// Recover finishes the durable selected transaction without starting a second authority.
//
//nolint:gocyclo // Recovery explicitly dispatches every durable phase and failure boundary.
func (engine *Engine) Recover(ctx context.Context) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	unlock, err := acquireInstallLock(engine.layout)
	if err != nil {
		return err
	}
	defer unlock()
	journal, err := LoadActiveJournal(engine.layout)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if journal.Phase == PhaseComplete || journal.Phase == PhaseRolledBack {
		return nil
	}
	if journal.Phase == PhaseRollingBack || journal.Phase == PhaseRollbackRetryable {
		return engine.continueRollback(ctx, journal)
	}
	if journal.Phase != PhasePointerCommitted && journal.Phase != PhaseReady && journal.Phase != PhaseRollForwardRetryable {
		return engine.rollback(ctx, journal, "incomplete_preselection", errors.New("incomplete pre-selection transaction"))
	}
	forwardOnly := journal.Phase == PhaseRollForwardRetryable
	if journal.Phase == PhasePointerCommitted {
		if !journal.ServiceTransitionAttempted {
			journal.ServiceTransitionAttempted = true
			if err := saveJournal(engine.layout, journal); err != nil {
				return engine.rollback(ctx, journal, "service_transition_journal_failed", err)
			}
		}
		installed := installedFromJournal(engine.layout, journal)
		if err := engine.service.VerifyCandidate(ctx, installed); err != nil {
			if err := engine.service.Restart(ctx); err != nil {
				return engine.rollback(ctx, journal, "candidate_restart_failed", err)
			}
		}
	}
	if err := engine.hooks.Ready(ctx, installedFromJournal(engine.layout, journal)); err != nil {
		if journal.Phase == PhaseReady || journal.Phase == PhaseRollForwardRetryable {
			return engine.markRollForwardRetryable(journal, "candidate_readiness_failed", err)
		}
		return engine.handleClassifiedFailure(ctx, journal, "candidate_readiness_failed", err)
	}
	if journal.Phase != PhaseReady && !forwardOnly {
		journal.Phase = PhaseReady
		journal.FailureCode = ""
		journal.RetryCode = ""
		if err := saveJournal(engine.layout, journal); err != nil {
			return engine.handleClassifiedFailure(ctx, journal, "ready_journal_failed", err)
		}
	}
	if err := engine.hooks.Commit(ctx); err != nil {
		if forwardOnly {
			return engine.markRollForwardRetryable(journal, "role_commit_failed", err)
		}
		return engine.handleClassifiedFailure(ctx, journal, "role_commit_failed", err)
	}
	journal.Phase = PhaseComplete
	journal.FailureCode = ""
	journal.RetryCode = ""
	return saveJournal(engine.layout, journal)
}

func (engine *Engine) requireRecoveredJournal() error {
	journal, err := LoadActiveJournal(engine.layout)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if journal.Phase == PhaseComplete || journal.Phase == PhaseRolledBack {
		return nil
	}
	return fmt.Errorf("%w: role=%s phase=%s", ErrRecoveryRequired, journal.Role, journal.Phase)
}

func (engine *Engine) handleClassifiedFailure(
	ctx context.Context,
	journal TransactionJournal,
	code string,
	cause error,
) error {
	classifier, ok := engine.hooks.(RoleFailureClassifier)
	if !ok {
		return engine.rollback(ctx, journal, code, cause)
	}
	disposition, err := classifier.FailureDisposition(ctx, PhaseReady)
	if err != nil {
		return engine.markRollForwardRetryable(
			journal,
			"failure_disposition_unknown",
			errors.Join(cause, fmt.Errorf("classify role readiness failure: %w", err)),
		)
	}
	switch disposition {
	case FailureDispositionRollback:
		return engine.rollback(ctx, journal, code, cause)
	case FailureDispositionRollForward:
		return engine.markRollForwardRetryable(journal, code, cause)
	default:
		return engine.markRollForwardRetryable(
			journal,
			"failure_disposition_unknown",
			errors.Join(cause, fmt.Errorf("unsupported role failure disposition %q", disposition)),
		)
	}
}

func (engine *Engine) markRollForwardRetryable(
	journal TransactionJournal,
	code string,
	cause error,
) error {
	journal.Phase = PhaseRollForwardRetryable
	journal.FailureCode = code
	journal.RetryCode = code
	if err := saveJournal(engine.layout, journal); err != nil {
		return errors.Join(cause, fmt.Errorf("persist retryable roll-forward: %w", err))
	}
	return cause
}

// Remove stops the exact role and removes only disposable role selection and releases.
func (engine *Engine) Remove(ctx context.Context) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	unlock, err := acquireInstallLock(engine.layout)
	if err != nil {
		return err
	}
	defer unlock()
	if err := engine.service.Stop(ctx); err != nil {
		return fmt.Errorf("stop %s service for removal: %w", engine.layout.Role, err)
	}
	if err := engine.service.Disable(ctx); err != nil {
		return fmt.Errorf("disable %s service for removal: %w", engine.layout.Role, err)
	}
	if err := engine.hooks.Remove(ctx); err != nil {
		return fmt.Errorf("remove %s role hooks: %w", engine.layout.Role, err)
	}
	if err := removeSelection(engine.layout.CurrentSelection); err != nil {
		return err
	}
	if err := makeTreeWritableForRemoval(engine.layout.ReleasesRoot); err != nil {
		return fmt.Errorf("prepare %s immutable releases for removal: %w", engine.layout.Role, err)
	}
	if err := os.RemoveAll(engine.layout.ReleasesRoot); err != nil {
		return fmt.Errorf("remove %s immutable releases: %w", engine.layout.Role, err)
	}
	return nil
}

// PlanPurge creates an exact offline plan bound to current preserved state.
func (engine *Engine) PlanPurge(ctx context.Context) (PurgePlan, error) {
	if err := ctx.Err(); err != nil {
		return PurgePlan{}, err
	}
	targetRevisions, err := purgeTargetRevisions(engine.purgeTargets)
	if err != nil {
		return PurgePlan{}, err
	}
	plan := PurgePlan{
		Role:            engine.layout.Role,
		Targets:         append([]string(nil), engine.purgeTargets...),
		TargetRevisions: targetRevisions,
		Exclusions:      append([]string(nil), engine.purgeExclusions...),
	}
	plan.Revision = purgePlanRevision(plan)
	return plan, nil
}

// ApplyPurge deletes only a still-current exact offline plan.
func (engine *Engine) ApplyPurge(ctx context.Context, plan PurgePlan) error {
	if plan.Role != engine.layout.Role || !equalStrings(plan.Targets, engine.purgeTargets) ||
		!equalStrings(plan.Exclusions, engine.purgeExclusions) {
		return errors.New("purge plan does not match the selected role-owned state")
	}
	if plan.Revision == "" || plan.Revision != purgePlanRevision(plan) {
		return errors.New("purge plan revision does not match its exact target inventory")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, target := range engine.purgeTargets {
		plannedRevision, ok := plan.TargetRevisions[target]
		if !ok || plannedRevision == "" {
			return errors.New("purge plan omits an exact target revision")
		}
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		current, err := directoryRevision(target)
		if err != nil {
			return err
		}
		if current != plannedRevision {
			return fmt.Errorf("%w: target=%s current=%s plan=%s", ErrRevisionConflict, target, current, plannedRevision)
		}
		if err := removePurgeTarget(target); err != nil {
			return fmt.Errorf("purge %s preserved state %q: %w", engine.layout.Role, target, err)
		}
	}
	return nil
}

// LoadActiveJournal reads the bounded exact role transaction journal.
//
//nolint:gocyclo // Journal validation fails closed over every durable phase/progress combination.
func LoadActiveJournal(layout RoleLayout) (TransactionJournal, error) {
	directory, err := openOwnedReleaseDirectory(layout.TransactionRoot, false)
	if err != nil {
		return TransactionJournal{}, err
	}
	defer func() { _ = directory.Close() }()
	body, err := readOwnedReleaseStateFile(directory, "active.json", maximumReleaseJournalBytes)
	if err != nil {
		return TransactionJournal{}, err
	}
	var journal TransactionJournal
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return TransactionJournal{}, fmt.Errorf("decode release transaction journal: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return TransactionJournal{}, errors.New("release transaction journal contains trailing data")
	}
	wantRelease, releaseErr := ReleaseID(journal.Version, journal.ContentIdentity)
	if journal.SchemaVersion != 1 || journal.Role != layout.Role || journal.ToRelease == "" ||
		releaseErr != nil || journal.ToRelease != wantRelease || journal.Executable != expectedRoleExecutable(journal.Role) ||
		!journal.ServiceStateObserved || journal.UpdatedAt <= 0 || !safeJournalReleaseID(journal.FromRelease) {
		return TransactionJournal{}, errors.New("release transaction journal has invalid authority")
	}
	switch journal.Phase {
	case PhaseStaged, PhasePrepared, PhasePointerCommitted, PhaseReady, PhaseRollingBack,
		PhaseRollbackRetryable, PhaseRolledBack, PhaseRollForwardRetryable, PhaseComplete:
	default:
		return TransactionJournal{}, errors.New("release transaction journal has unsupported phase")
	}
	if (journal.Phase == PhaseReady || journal.Phase == PhaseRollForwardRetryable || journal.Phase == PhaseComplete) &&
		!journal.ServiceTransitionAttempted {
		return TransactionJournal{}, errors.New("release transaction journal omitted required service transition intent")
	}
	if (journal.Phase == PhaseStaged || journal.Phase == PhasePrepared) && journal.ServiceTransitionAttempted {
		return TransactionJournal{}, errors.New("release transaction journal has service transition intent before selection")
	}
	if journal.Phase == PhaseRollingBack && (journal.FailureCode == "" || journal.RetryCode != "") {
		return TransactionJournal{}, errors.New("release transaction journal has invalid rollback intent metadata")
	}
	if journal.Phase == PhaseRollbackRetryable && (journal.FailureCode == "" || journal.RetryCode == "") {
		return TransactionJournal{}, errors.New("release transaction journal has incomplete retryable rollback")
	}
	rollbackProgress := journal.CandidateStopped || journal.CandidateDisabled || journal.SelectionRestored ||
		journal.HooksRestored || journal.ServiceSurfaceReloaded || journal.PriorServiceRestored ||
		journal.ServiceEnablementRestored
	if journal.Phase != PhaseRollingBack && journal.Phase != PhaseRollbackRetryable &&
		journal.Phase != PhaseRolledBack && rollbackProgress {
		return TransactionJournal{}, errors.New("release transaction journal has rollback progress outside rollback")
	}
	if journal.CandidateDisabled && !journal.CandidateStopped || journal.SelectionRestored && !journal.CandidateDisabled ||
		journal.HooksRestored && !journal.SelectionRestored || journal.ServiceSurfaceReloaded && !journal.HooksRestored ||
		journal.PriorServiceRestored && !journal.ServiceSurfaceReloaded ||
		journal.ServiceEnablementRestored && !journal.PriorServiceRestored {
		return TransactionJournal{}, errors.New("release transaction journal has out-of-order rollback progress")
	}
	if journal.Phase == PhaseRollForwardRetryable &&
		(journal.FailureCode == "" || journal.RetryCode == "" || rollbackProgress) {
		return TransactionJournal{}, errors.New("release transaction journal has incomplete retryable roll-forward")
	}
	if journal.Phase != PhaseRollingBack && journal.Phase != PhaseRollbackRetryable &&
		journal.Phase != PhaseRolledBack && journal.Phase != PhaseRollForwardRetryable &&
		(journal.FailureCode != "" || journal.RetryCode != "") {
		return TransactionJournal{}, errors.New("release transaction journal has failure metadata outside recovery")
	}
	if journal.Phase == PhaseRolledBack && (!journal.CandidateStopped || !journal.CandidateDisabled ||
		!journal.SelectionRestored || !journal.HooksRestored || !journal.ServiceSurfaceReloaded ||
		!journal.PriorServiceRestored || !journal.ServiceEnablementRestored || journal.FailureCode == "" || journal.RetryCode != "") {
		return TransactionJournal{}, errors.New("release transaction journal has incomplete terminal rollback")
	}
	if len(journal.FailureCode) > 64 || len(journal.RetryCode) > 64 {
		return TransactionJournal{}, errors.New("release transaction journal has unbounded failure metadata")
	}
	if journal.Phase != PhaseComplete && journal.Phase != PhaseRolledBack {
		if err := validateJournalPriorRelease(layout, journal); err != nil {
			return TransactionJournal{}, err
		}
	}
	return journal, nil
}

func safeJournalReleaseID(releaseID string) bool {
	if releaseID == "" {
		return true
	}
	if len(releaseID) > 192 || filepath.Base(releaseID) != releaseID || strings.ContainsAny(releaseID, " /\\\t\r\n") {
		return false
	}
	separator := strings.LastIndexByte(releaseID, '-')
	if separator < 1 || !safeVersion.MatchString(releaseID[:separator]) || len(releaseID[separator+1:]) != 16 ||
		strings.ToLower(releaseID[separator+1:]) != releaseID[separator+1:] {
		return false
	}
	_, err := hex.DecodeString(releaseID[separator+1:])
	return err == nil
}

func validateJournalPriorRelease(layout RoleLayout, journal TransactionJournal) error {
	if journal.FromRelease == "" {
		return nil
	}
	root := filepath.Join(layout.ReleasesRoot, journal.FromRelease)
	source, err := openSecureReleaseSource(root)
	if err != nil {
		return errors.New("release transaction prior release is absent or indirect")
	}
	defer func() { _ = source.close() }()
	body, err := source.readRegular("manifest.json", 2, 1<<20)
	if err != nil {
		return errors.New("release transaction prior release manifest is unavailable")
	}
	var manifest sourceReleaseManifest
	if err := decodeSourceReleaseManifest(body, &manifest); err != nil {
		return errors.New("release transaction prior release manifest is invalid")
	}
	identity, err := secureReleaseContentIdentity(source)
	if err != nil {
		return err
	}
	request := InstallRequest{
		Version: manifest.ReleaseVersion, ContentIdentity: identity, SourceRoot: root,
		Executable: expectedRoleExecutable(layout.Role),
	}
	if want, releaseErr := ReleaseID(request.Version, request.ContentIdentity); releaseErr != nil || want != journal.FromRelease {
		return errors.New("release transaction prior release identity does not match its immutable tree")
	}
	if err := validateInstallSource(source, request, false); err != nil {
		return fmt.Errorf("validate release transaction prior release: %w", err)
	}
	return source.walk(func(relative string, info os.FileInfo, _ *os.File) error {
		if info.Mode().Perm()&0o222 != 0 {
			return fmt.Errorf("release transaction prior release path %q became writable", relative)
		}
		return nil
	})
}

func (engine *Engine) rollback(
	ctx context.Context,
	journal TransactionJournal,
	failureCode string,
	cause error,
) error {
	journal.Phase = PhaseRollingBack
	journal.FailureCode = failureCode
	journal.RetryCode = ""
	if err := saveJournal(engine.layout, journal); err != nil {
		return errors.Join(cause, fmt.Errorf("persist rollback intent: %w", err))
	}
	if engine.crashPoint == PhaseRollingBack {
		return errors.Join(cause, ErrInjectedCrash)
	}
	return errors.Join(cause, engine.continueRollback(ctx, journal))
}

// continueRollback resumes only rollback steps once rollback intent is
// durable. It never starts or verifies the failed candidate. Each successful
// step is checkpointed before a later step can restore an authority.
//
//nolint:gocyclo // Each durable rollback checkpoint is intentionally visible and independently retryable.
func (engine *Engine) continueRollback(ctx context.Context, journal TransactionJournal) error {
	journal.Phase = PhaseRollingBack
	journal.RetryCode = ""
	if err := saveJournal(engine.layout, journal); err != nil {
		return fmt.Errorf("persist resumed rollback intent: %w", err)
	}
	if !journal.CandidateStopped {
		if journal.ServiceTransitionAttempted {
			if err := engine.service.Stop(ctx); err != nil {
				return engine.rollbackRetryable(journal, "candidate_stop_failed", fmt.Errorf("stop failed candidate: %w", err))
			}
		}
		journal.CandidateStopped = true
		if err := saveJournal(engine.layout, journal); err != nil {
			return fmt.Errorf("checkpoint stopped candidate: %w", err)
		}
	}
	if !journal.CandidateDisabled {
		if journal.ServiceTransitionAttempted && journal.FromRelease == "" {
			if err := engine.service.Disable(ctx); err != nil {
				return engine.rollbackRetryable(journal, "candidate_disable_failed", fmt.Errorf("disable failed candidate: %w", err))
			}
		}
		journal.CandidateDisabled = true
		if err := saveJournal(engine.layout, journal); err != nil {
			return fmt.Errorf("checkpoint disabled candidate: %w", err)
		}
	}
	if !journal.SelectionRestored {
		var err error
		if journal.FromRelease == "" {
			err = removeSelection(engine.layout.CurrentSelection)
		} else {
			err = selectRelease(engine.layout, journal.FromRelease)
		}
		if err != nil {
			return engine.rollbackRetryable(journal, "selection_restore_failed", fmt.Errorf("restore prior selection: %w", err))
		}
		journal.SelectionRestored = true
		if err := saveJournal(engine.layout, journal); err != nil {
			return fmt.Errorf("checkpoint restored selection: %w", err)
		}
	}
	if !journal.HooksRestored {
		if err := engine.hooks.Rollback(ctx); err != nil {
			return engine.rollbackRetryable(journal, "role_hooks_restore_failed", fmt.Errorf("restore prior role hooks: %w", err))
		}
		journal.HooksRestored = true
		if err := saveJournal(engine.layout, journal); err != nil {
			return fmt.Errorf("checkpoint restored role hooks: %w", err)
		}
	}
	if !journal.ServiceSurfaceReloaded {
		if err := engine.service.Reload(ctx); err != nil {
			return engine.rollbackRetryable(journal, "service_surface_reload_failed", fmt.Errorf("reload restored service surface: %w", err))
		}
		journal.ServiceSurfaceReloaded = true
		if err := saveJournal(engine.layout, journal); err != nil {
			return fmt.Errorf("checkpoint reloaded restored service surface: %w", err)
		}
	}
	if !journal.PriorServiceRestored {
		if journal.ServiceTransitionAttempted && journal.FromRelease != "" && journal.ServiceWasRunning {
			if err := engine.service.Verify(ctx); err != nil {
				if err := engine.service.Start(ctx); err != nil {
					return engine.rollbackRetryable(journal, "prior_service_start_failed", fmt.Errorf("start prior service: %w", err))
				}
				if err := engine.service.Verify(ctx); err != nil {
					return engine.rollbackRetryable(journal, "prior_service_verify_failed", fmt.Errorf("verify prior service: %w", err))
				}
			}
		}
		journal.PriorServiceRestored = true
		if err := saveJournal(engine.layout, journal); err != nil {
			return fmt.Errorf("checkpoint restored prior service: %w", err)
		}
	}
	if !journal.ServiceEnablementRestored {
		if journal.ServiceTransitionAttempted && journal.FromRelease != "" {
			var err error
			if journal.ServiceWasEnabled {
				err = engine.service.Enable(ctx)
			} else {
				err = engine.service.Disable(ctx)
			}
			if err != nil {
				return engine.rollbackRetryable(journal, "prior_enablement_restore_failed", fmt.Errorf("restore prior service enablement: %w", err))
			}
		}
		journal.ServiceEnablementRestored = true
		if err := saveJournal(engine.layout, journal); err != nil {
			return fmt.Errorf("checkpoint restored prior service enablement: %w", err)
		}
	}
	journal.Phase = PhaseRolledBack
	journal.RetryCode = ""
	if err := saveJournal(engine.layout, journal); err != nil {
		return fmt.Errorf("commit terminal rollback journal: %w", err)
	}
	return nil
}

func (engine *Engine) rollbackRetryable(journal TransactionJournal, code string, cause error) error {
	journal.Phase = PhaseRollbackRetryable
	journal.RetryCode = code
	if err := saveJournal(engine.layout, journal); err != nil {
		return errors.Join(cause, fmt.Errorf("persist retryable rollback: %w", err))
	}
	return cause
}

func validateInstallRequest(request InstallRequest) error {
	if _, err := ReleaseID(request.Version, request.ContentIdentity); err != nil {
		return err
	}
	if request.Executable != "agent-sessions" && request.Executable != "agent-sessions-hub" {
		return fmt.Errorf("unsupported release executable %q", request.Executable)
	}
	source, err := openSecureReleaseSource(request.SourceRoot)
	if err != nil {
		return err
	}
	defer func() { _ = source.close() }()
	return validateInstallSource(source, request, true)
}

//nolint:gocyclo // Validation keeps executable, manifest, inventory, identity, and checksum authority together.
func validateInstallSource(source *secureReleaseSource, request InstallRequest, verifyIdentity bool) error {
	executablePath := filepath.ToSlash(filepath.Join("bin", request.Executable))
	executable, executableInfo, err := source.open(executablePath)
	if err != nil || !executableInfo.Mode().IsRegular() || executableInfo.Mode()&0o111 == 0 ||
		executableInfo.Size() < 1 || executableInfo.Size() > maximumReleaseSourceFile {
		if executable != nil {
			_ = executable.Close()
		}
		return fmt.Errorf("release executable %q is missing or not executable", request.Executable)
	}
	if err := executable.Close(); err != nil {
		return err
	}
	var manifest sourceReleaseManifest
	body, err := source.readRegular("manifest.json", 2, 1<<20)
	if err != nil || decodeSourceReleaseManifest(body, &manifest) != nil {
		return errors.New("release manifest does not match requested version and content identity")
	}
	if manifest.SchemaVersion != 1 || manifest.ReleaseVersion != request.Version ||
		manifest.HubProtocolVersion != productcatalog.ProtocolVersion || manifest.Platform != currentReleasePlatform() ||
		manifest.Checksums != "SHA256SUMS" || !sourceReleaseManifestHasExecutable(manifest.Executables, request.Executable) {
		return errors.New("source release manifest does not match the requested role release")
	}
	if err := validateSourceReleaseRoleInventory(source, request.Executable, manifest); err != nil {
		return err
	}
	if verifyIdentity {
		identity, err := secureReleaseContentIdentity(source)
		if err != nil || identity != request.ContentIdentity {
			return errors.New("source release payload does not match its requested content identity")
		}
	}
	if err := verifySourceReleaseChecksumsAt(source, manifest.Checksums); err != nil {
		return err
	}
	return nil
}

func decodeSourceReleaseManifest(body []byte, manifest *sourceReleaseManifest) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(manifest); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("source release manifest contains trailing data")
	}
	return nil
}

func currentReleasePlatform() string {
	architecture := runtime.GOARCH
	if architecture == "amd64" {
		architecture = "x64"
	}
	return runtime.GOOS + "-" + architecture
}

func sourceReleaseManifestHasExecutable(executables []sourceReleaseExecutable, executable string) bool {
	if len(executables) != 1 {
		return false
	}
	wantRole := RoleHost
	if executable == "agent-sessions-hub" {
		wantRole = RoleHub
	}
	wantPath := filepath.ToSlash(filepath.Join("bin", executable))
	for _, entry := range executables {
		if entry.Name == executable && entry.Role == wantRole && entry.Path == wantPath {
			return true
		}
	}
	return false
}

var canonicalConnectorPayloads = []sourceReleaseConnector{
	{Product: "codex", PluginID: "agent-sessions", ArchivePaths: []string{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills"}},
	{Product: "claude", PluginID: "agent-sessions", ArchivePaths: []string{".claude-plugin", "claude"}},
	{Product: "grok", PluginID: "agent-sessions", ArchivePaths: []string{"grok"}},
	{Product: "qwen", PluginID: "agent-sessions", ArchivePaths: []string{"qwen"}},
}

var canonicalRoleServiceAssets = map[Role][]string{
	RoleHost: {
		"deploy/agent-sessions/systemd/user/agent-sessions.service",
		"deploy/agent-sessions/launchd/net.antst.agent-sessions.plist",
	},
	RoleHub: {
		"deploy/agent-sessions-hub/systemd/user/agent-sessions-hub.service",
		"deploy/agent-sessions-hub/systemd/user/hub.env.example",
		"deploy/agent-sessions-hub/launchd/net.antst.agent-sessions-hub.plist",
	},
}

func validateSourceReleaseRoleInventory(source *secureReleaseSource, executable string, manifest sourceReleaseManifest) error {
	if executable == "agent-sessions" {
		if !equalSourceReleaseConnectors(manifest.ConnectorPayloads, canonicalConnectorPayloads) ||
			!equalStrings(manifest.ServiceAssets.Host, canonicalRoleServiceAssets[RoleHost]) ||
			len(manifest.ServiceAssets.Hub) != 0 {
			return errors.New("host source release manifest has an incomplete or cross-role inventory")
		}
		for _, connector := range manifest.ConnectorPayloads {
			for _, path := range connector.ArchivePaths {
				if err := validateSourceReleaseDeclaredPath(source, path, path != ".mcp.json"); err != nil {
					return err
				}
			}
		}
		for _, path := range manifest.ServiceAssets.Host {
			if err := validateSourceReleaseDeclaredPath(source, path, false); err != nil {
				return err
			}
		}
		return nil
	}
	if len(manifest.ConnectorPayloads) != 0 || len(manifest.ServiceAssets.Host) != 0 ||
		!equalStrings(manifest.ServiceAssets.Hub, canonicalRoleServiceAssets[RoleHub]) {
		return errors.New("hub source release manifest has an incomplete or cross-role inventory")
	}
	for _, path := range manifest.ServiceAssets.Hub {
		if err := validateSourceReleaseDeclaredPath(source, path, false); err != nil {
			return err
		}
	}
	return nil
}

func equalSourceReleaseConnectors(left, right []sourceReleaseConnector) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Product != right[index].Product || left[index].PluginID != right[index].PluginID ||
			!equalStrings(left[index].ArchivePaths, right[index].ArchivePaths) {
			return false
		}
	}
	return true
}

func validateSourceReleaseDeclaredPath(source *secureReleaseSource, declared string, wantDirectory bool) error {
	file, info, err := source.open(declared)
	if err != nil || (wantDirectory && !info.IsDir()) || (!wantDirectory && !info.Mode().IsRegular()) {
		if file != nil {
			_ = file.Close()
		}
		return fmt.Errorf("source release manifest payload %q is absent or indirect", declared)
	}
	return file.Close()
}

func verifySourceReleaseChecksums(root, checksumName string) error {
	source, err := openSecureReleaseSource(root)
	if err != nil {
		return err
	}
	defer func() { _ = source.close() }()
	return verifySourceReleaseChecksumsAt(source, checksumName)
}

//nolint:gocyclo // One closed descriptor traversal verifies every checksum row and source entry.
func verifySourceReleaseChecksumsAt(source *secureReleaseSource, checksumName string) error {
	body, err := source.readRegular(checksumName, 1, 16<<20)
	if err != nil {
		return errors.New("source release checksum manifest is absent or unbounded")
	}
	want := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || len(parts[0]) != 64 {
			return errors.New("source release checksum manifest has an invalid row")
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return errors.New("source release checksum manifest has a non-hexadecimal digest")
		}
		relative, err := canonicalReleaseRelativePath(parts[1])
		if err != nil || filepath.ToSlash(relative) == checksumName {
			return errors.New("source release checksum manifest has an unsafe path")
		}
		canonical := filepath.ToSlash(relative)
		if _, duplicate := want[canonical]; duplicate {
			return errors.New("source release checksum manifest repeats a path")
		}
		want[canonical] = strings.ToLower(parts[0])
	}
	seen := make(map[string]struct{}, len(want))
	err = source.walk(func(relative string, info os.FileInfo, file *os.File) error {
		if info.IsDir() {
			return nil
		}
		if relative == checksumName {
			return nil
		}
		expected, exists := want[relative]
		if !exists {
			return fmt.Errorf("source release checksum manifest omits %q", relative)
		}
		hash := sha256.New()
		written, copyErr := io.CopyN(hash, file, info.Size()+1)
		if !errors.Is(copyErr, io.EOF) || written != info.Size() {
			return fmt.Errorf("source release file %q changed while checksumming", relative)
		}
		actual := hex.EncodeToString(hash.Sum(nil))
		if actual != expected {
			return fmt.Errorf("source release checksum mismatch for %q", relative)
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(want) {
		return errors.New("source release checksum manifest names an absent path")
	}
	return nil
}

func stageImmutableRelease(request InstallRequest, destination string) error {
	if info, err := os.Lstat(destination); err == nil {
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return validateExistingImmutableRelease(request, destination)
		}
		return errors.New("existing release identity changed filesystem type")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".stage-*")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	identity, err := copySecureReleaseTree(request.SourceRoot, temporary)
	if err != nil {
		return err
	}
	if identity != request.ContentIdentity {
		return errors.New("release source changed while staging its exact payload")
	}
	stagedSource, err := openSecureReleaseSource(temporary)
	if err != nil {
		return err
	}
	stagedRequest := request
	stagedRequest.SourceRoot = temporary
	validationErr := validateInstallSource(stagedSource, stagedRequest, false)
	closeErr := stagedSource.close()
	if err := errors.Join(validationErr, closeErr); err != nil {
		return fmt.Errorf("validate exact staged release: %w", err)
	}
	if err := makeTreeImmutable(temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	committed = true
	return syncDirectory(filepath.Dir(destination))
}

func validateExistingImmutableRelease(request InstallRequest, root string) error {
	source, err := openSecureReleaseSource(root)
	if err != nil {
		return err
	}
	defer func() { _ = source.close() }()
	stagedRequest := request
	stagedRequest.SourceRoot = root
	if err := validateInstallSource(source, stagedRequest, false); err != nil {
		return fmt.Errorf("existing immutable release does not match its selected manifest: %w", err)
	}
	identity, err := secureReleaseContentIdentity(source)
	if err != nil {
		return fmt.Errorf("hash existing immutable release: %w", err)
	}
	if identity != request.ContentIdentity {
		return errors.New("existing immutable release does not match its selected content identity")
	}
	return source.walk(func(relative string, info os.FileInfo, _ *os.File) error {
		if info.Mode().Perm()&0o222 != 0 {
			return fmt.Errorf("existing immutable release path %q became writable", relative)
		}
		return nil
	})
}

func makeTreeImmutable(root string) error {
	return makeTreeImmutableWithHook(root, nil)
}

func makeTreeImmutableWithHook(root string, beforeChmod func(string)) error {
	source, err := openSecureReleaseSource(root)
	if err != nil {
		return err
	}
	defer func() { _ = source.close() }()
	if err := source.walk(func(relative string, info os.FileInfo, file *os.File) error {
		if beforeChmod != nil {
			beforeChmod(relative)
		}
		mode := os.FileMode(0o444)
		if info.IsDir() || info.Mode()&0o111 != 0 {
			mode = 0o555
		}
		return file.Chmod(mode)
	}); err != nil {
		return err
	}
	return source.directory.Chmod(0o555)
}

func makeTreeWritableForRemoval(root string) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return makeTreeWritableForRemovalWithHook(root, nil)
}

func makeTreeWritableForRemovalWithHook(root string, beforeChmod func(string)) error {
	source, err := openSecureReleaseSource(root)
	if err != nil {
		return err
	}
	defer func() { _ = source.close() }()
	if err := source.walk(func(relative string, info os.FileInfo, file *os.File) error {
		if beforeChmod != nil {
			beforeChmod(relative)
		}
		mode := os.FileMode(0o600)
		if info.IsDir() {
			mode = 0o700
		} else if info.Mode()&0o111 != 0 {
			mode = 0o700
		}
		return file.Chmod(mode)
	}); err != nil {
		return err
	}
	return source.directory.Chmod(0o700)
}

func selectRelease(layout RoleLayout, releaseID string) error {
	if filepath.Base(releaseID) != releaseID || releaseID == "." {
		return errors.New("release selection identity is unsafe")
	}
	if err := os.MkdirAll(filepath.Dir(layout.CurrentSelection), 0o700); err != nil {
		return err
	}
	temporary := layout.CurrentSelection + ".new"
	_ = os.Remove(temporary)
	if err := os.Symlink(filepath.Join("releases", releaseID), temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, layout.CurrentSelection); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(filepath.Dir(layout.CurrentSelection))
}

func currentReleaseID(layout RoleLayout) (string, error) {
	target, err := os.Readlink(layout.CurrentSelection)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read current %s release: %w", layout.Role, err)
	}
	releaseID := filepath.Base(target)
	if target != filepath.Join("releases", releaseID) || !safeJournalReleaseID(releaseID) {
		return "", errors.New("current release selection is not role-relative")
	}
	return releaseID, nil
}

func removeSelection(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return errors.New("current release selection changed type")
	}
	return os.Remove(path)
}

func saveJournal(layout RoleLayout, journal TransactionJournal) error {
	journal.UpdatedAt = time.Now().UnixMilli()
	directory, err := openOwnedReleaseDirectory(layout.TransactionRoot, true)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeOwnedReleaseStateFile(directory, "active.json", body)
}

func installedFromJournal(layout RoleLayout, journal TransactionJournal) InstalledRelease {
	return InstalledRelease{
		Role: layout.Role, ReleaseID: journal.ToRelease, Version: journal.Version,
		ContentIdentity: journal.ContentIdentity, Root: filepath.Join(layout.ReleasesRoot, journal.ToRelease),
		Executable: filepath.Join(layout.ReleasesRoot, journal.ToRelease, "bin", journal.Executable),
	}
}

func acquireInstallLock(layout RoleLayout) (func(), error) {
	directory, err := openOwnedReleaseDirectory(layout.ReleaseRoot, true)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(
		int(directory.Fd()), filepath.Base(layout.InstallLock),
		unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o600,
	)
	_ = directory.Close()
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), layout.InstallLock)
	wantUID, uidErr := currentReleaseUID()
	if uidErr != nil {
		_ = file.Close()
		return nil, uidErr
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Uid != wantUID || stat.Mode&0o777 != 0o600 {
		_ = file.Close()
		return nil, errors.New("release install lock is not a private current-user-owned regular file")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func directoryRevision(root string) (string, error) {
	hash := sha256.New()
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		_, _ = hash.Write([]byte("absent\n"))
		return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
	} else if err != nil {
		return "", err
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("preserved state contains a symlink")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%d\n", relative, info.Mode(), info.Size())
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path) //nolint:gosec // Walk is confined to Agent Sessions-owned preserved state.
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		return errors.Join(copyErr, file.Close())
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func validatePurgeRoots(targets, exclusions []string) error {
	seen := make(map[string]struct{}, len(targets))
	for _, exclusion := range exclusions {
		if !cleanAbsoluteNonRoot(exclusion) {
			return fmt.Errorf("purge exclusion %q is not clean and absolute", exclusion)
		}
	}
	for _, target := range targets {
		if !cleanAbsoluteNonRoot(target) {
			return fmt.Errorf("purge target %q is not clean and absolute", target)
		}
		if _, duplicate := seen[target]; duplicate {
			return fmt.Errorf("duplicate purge target %q", target)
		}
		seen[target] = struct{}{}
		for _, exclusion := range exclusions {
			if pathWithinRoot(target, exclusion) {
				return fmt.Errorf("purge target %q is within excluded root %q", target, exclusion)
			}
		}
	}
	return nil
}

func purgeTargetRevisions(targets []string) (map[string]string, error) {
	revisions := make(map[string]string, len(targets))
	for _, target := range targets {
		revision, err := directoryRevision(target)
		if err != nil {
			return nil, err
		}
		revisions[target] = revision
	}
	return revisions, nil
}

func purgePlanRevision(plan PurgePlan) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\n", plan.Role)
	for _, target := range plan.Targets {
		_, _ = fmt.Fprintf(hash, "target\x00%s\x00%s\n", target, plan.TargetRevisions[target])
	}
	for _, exclusion := range plan.Exclusions {
		_, _ = fmt.Fprintf(hash, "exclude\x00%s\n", exclusion)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func removePurgeTarget(target string) error {
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("purge target changed filesystem type")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return errors.New("purge target is not owned by the current user")
	}
	return os.RemoveAll(target)
}

func cleanAbsoluteNonRoot(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

func pathWithinRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // Canonical role-owned directory.
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
