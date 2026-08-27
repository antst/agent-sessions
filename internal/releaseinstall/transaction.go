package releaseinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

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
	// PhaseComplete is the committed terminal transaction state.
	PhaseComplete Phase = "complete"
)

var (
	// ErrInjectedCrash identifies a test-owned crash boundary.
	ErrInjectedCrash = errors.New("injected release transaction crash")
	// ErrRevisionConflict rejects an offline purge plan after state changed.
	ErrRevisionConflict = errors.New("release state revision conflict")
)

// InstallRequest identifies a complete validated role release source.
type InstallRequest struct {
	Version         string
	ContentIdentity string
	SourceRoot      string
	Executable      string
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
	SchemaVersion   int    `json:"schema_version"`
	Role            Role   `json:"role"`
	Phase           Phase  `json:"phase"`
	FromRelease     string `json:"from_release,omitempty"`
	ToRelease       string `json:"to_release"`
	Version         string `json:"version"`
	ContentIdentity string `json:"content_identity"`
	Executable      string `json:"executable"`
	UpdatedAt       int64  `json:"updated_at"`
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
	unlock, err := acquireInstallLock(engine.layout)
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()

	if err := validateInstallRequest(request); err != nil {
		return InstallResult{}, err
	}
	releaseID, _ := ReleaseID(request.Version, request.ContentIdentity)
	prior, err := currentReleaseID(engine.layout)
	if err != nil {
		return InstallResult{}, err
	}
	journal := TransactionJournal{
		SchemaVersion: 1, Role: engine.layout.Role, Phase: PhaseStaged, FromRelease: prior,
		ToRelease: releaseID, Version: request.Version, ContentIdentity: request.ContentIdentity,
		Executable: request.Executable, UpdatedAt: time.Now().UnixMilli(),
	}
	releaseRoot := filepath.Join(engine.layout.ReleasesRoot, releaseID)
	if err := stageImmutableRelease(request.SourceRoot, releaseRoot); err != nil {
		return InstallResult{}, fmt.Errorf("stage immutable %s release: %w", engine.layout.Role, err)
	}
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, err
	}
	hookRequest := request
	hookRequest.SourceRoot = releaseRoot
	if err := engine.hooks.Prepare(ctx, hookRequest); err != nil {
		return InstallResult{}, fmt.Errorf("prepare %s release hooks: %w", engine.layout.Role, err)
	}
	journal.Phase = PhasePrepared
	if err := saveJournal(engine.layout, journal); err != nil {
		_ = engine.hooks.Rollback(ctx)
		return InstallResult{}, err
	}
	if err := selectRelease(engine.layout, releaseID); err != nil {
		_ = engine.hooks.Rollback(ctx)
		return InstallResult{}, err
	}
	journal.Phase = PhasePointerCommitted
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, err
	}
	if engine.crashPoint == PhasePointerCommitted {
		return InstallResult{}, ErrInjectedCrash
	}
	installed := installedFromJournal(engine.layout, journal)
	if err := engine.service.Restart(ctx); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, fmt.Errorf("restart candidate service: %w", err))
	}
	if err := engine.hooks.Ready(ctx, installed); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, fmt.Errorf("candidate readiness: %w", err))
	}
	journal.Phase = PhaseReady
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, err)
	}
	if err := engine.hooks.Commit(ctx); err != nil {
		return InstallResult{}, engine.rollback(ctx, journal, fmt.Errorf("commit role hooks: %w", err))
	}
	journal.Phase = PhaseComplete
	if err := saveJournal(engine.layout, journal); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Role: engine.layout.Role, ReleaseID: releaseID, Phase: PhaseComplete}, nil
}

// Recover finishes the durable selected transaction without starting a second authority.
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
	if journal.Phase == PhaseComplete {
		return nil
	}
	if journal.Phase != PhasePointerCommitted && journal.Phase != PhaseReady {
		return engine.rollback(ctx, journal, errors.New("incomplete pre-selection transaction"))
	}
	if err := engine.service.Restart(ctx); err != nil {
		return engine.rollback(ctx, journal, err)
	}
	if err := engine.hooks.Ready(ctx, installedFromJournal(engine.layout, journal)); err != nil {
		return engine.rollback(ctx, journal, err)
	}
	if err := engine.hooks.Commit(ctx); err != nil {
		return engine.rollback(ctx, journal, err)
	}
	journal.Phase = PhaseComplete
	return saveJournal(engine.layout, journal)
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
func LoadActiveJournal(layout RoleLayout) (TransactionJournal, error) {
	body, err := os.ReadFile(filepath.Join(layout.TransactionRoot, "active.json"))
	if err != nil {
		return TransactionJournal{}, err
	}
	var journal TransactionJournal
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return TransactionJournal{}, fmt.Errorf("decode release transaction journal: %w", err)
	}
	if journal.SchemaVersion != 1 || journal.Role != layout.Role || journal.ToRelease == "" {
		return TransactionJournal{}, errors.New("release transaction journal has invalid authority")
	}
	return journal, nil
}

func (engine *Engine) rollback(ctx context.Context, journal TransactionJournal, cause error) error {
	errorsList := []error{cause}
	if err := engine.service.Stop(ctx); err != nil {
		errorsList = append(errorsList, fmt.Errorf("stop failed candidate: %w", err))
	}
	if journal.FromRelease == "" {
		if err := removeSelection(engine.layout.CurrentSelection); err != nil {
			errorsList = append(errorsList, err)
		}
	} else if err := selectRelease(engine.layout, journal.FromRelease); err != nil {
		errorsList = append(errorsList, fmt.Errorf("restore prior selection: %w", err))
	} else {
		if err := engine.service.Start(ctx); err != nil {
			errorsList = append(errorsList, fmt.Errorf("start prior service: %w", err))
		} else if err := engine.service.Verify(ctx); err != nil {
			errorsList = append(errorsList, fmt.Errorf("verify prior service: %w", err))
		}
	}
	if err := engine.hooks.Rollback(ctx); err != nil {
		errorsList = append(errorsList, fmt.Errorf("restore prior role hooks: %w", err))
	}
	return errors.Join(errorsList...)
}

//nolint:gocyclo // Validation proves the complete source/manifest/executable identity before mutation.
func validateInstallRequest(request InstallRequest) error {
	if _, err := ReleaseID(request.Version, request.ContentIdentity); err != nil {
		return err
	}
	if !filepath.IsAbs(request.SourceRoot) || filepath.Clean(request.SourceRoot) != request.SourceRoot {
		return errors.New("release source must be clean and absolute")
	}
	info, err := os.Lstat(request.SourceRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release source is not a real directory")
	}
	if request.Executable != "agent-sessions" && request.Executable != "agent-sessions-hub" {
		return fmt.Errorf("unsupported release executable %q", request.Executable)
	}
	executable := filepath.Join(request.SourceRoot, "bin", request.Executable)
	info, err = os.Lstat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("release executable %q is missing or not executable", request.Executable)
	}
	var manifest struct {
		Version         string `json:"version"`
		ContentIdentity string `json:"content_identity"`
	}
	body, err := os.ReadFile(filepath.Join(request.SourceRoot, "manifest.json"))
	if err != nil || json.Unmarshal(body, &manifest) != nil || manifest.Version != request.Version || manifest.ContentIdentity != request.ContentIdentity {
		return errors.New("release manifest does not match requested version and content identity")
	}
	return nil
}

func stageImmutableRelease(source, destination string) error {
	if info, err := os.Lstat(destination); err == nil {
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return nil
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
	if err := copyReleaseTree(source, temporary); err != nil {
		return err
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

func copyReleaseTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("release source escaped its root")
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("release source contains symlink %q", relative)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release source contains unsupported type %q", relative)
		}
		input, err := os.Open(path) //nolint:gosec // Walk is confined to the validated source root.
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if info.Mode()&0o111 != 0 {
			mode = 0o700
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // Target is a validated relative path below the fresh release stage.
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		syncErr := output.Sync()
		return errors.Join(copyErr, syncErr, output.Close(), input.Close())
	})
}

func makeTreeImmutable(root string) error {
	var entries []string
	if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err == nil {
			entries = append(entries, path)
		}
		return err
	}); err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool { return len(entries[left]) > len(entries[right]) })
	for _, path := range entries {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if info.IsDir() || info.Mode()&0o111 != 0 {
			mode = 0o555
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	return nil
}

func makeTreeWritableForRemoval(root string) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if info.IsDir() {
			mode = 0o700
		} else if info.Mode()&0o111 != 0 {
			mode = 0o700
		}
		return os.Chmod(path, mode) //nolint:gosec // Exact immutable role tree is made owner-writable immediately before supported removal.
	})
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
	wantParent := "releases" + string(filepath.Separator)
	if !strings.HasPrefix(target, wantParent) || filepath.Base(target) == "." {
		return "", errors.New("current release selection is not role-relative")
	}
	return filepath.Base(target), nil
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
	if err := os.MkdirAll(layout.TransactionRoot, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(layout.TransactionRoot, ".journal-*")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer func() { _ = os.Remove(path) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(layout.TransactionRoot, "active.json")); err != nil {
		return err
	}
	return syncDirectory(layout.TransactionRoot)
}

func installedFromJournal(layout RoleLayout, journal TransactionJournal) InstalledRelease {
	return InstalledRelease{
		Role: layout.Role, ReleaseID: journal.ToRelease, Version: journal.Version,
		ContentIdentity: journal.ContentIdentity, Root: filepath.Join(layout.ReleasesRoot, journal.ToRelease),
		Executable: filepath.Join(layout.ReleasesRoot, journal.ToRelease, "bin", journal.Executable),
	}
}

func acquireInstallLock(layout RoleLayout) (func(), error) {
	if err := os.MkdirAll(layout.ReleaseRoot, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(layout.InstallLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
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
