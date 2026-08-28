package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/releaseinstall"
)

const (
	connectorRecoverySchemaVersion = 1
	maxConnectorRecoveryBytes      = int64(32 << 20)
)

type connectorRecoveryPhase string

const (
	connectorRecoveryPreparing   connectorRecoveryPhase = "preparing"
	connectorRecoveryPrepared    connectorRecoveryPhase = "prepared"
	connectorRecoveryCommitting  connectorRecoveryPhase = "committing"
	connectorRecoveryRollingBack connectorRecoveryPhase = "rolling_back"
	connectorRecoveryCommitted   connectorRecoveryPhase = "committed"
	connectorRecoveryRolledBack  connectorRecoveryPhase = "rolled_back"
)

type connectorProductProgress string

const (
	connectorProductPrepared   connectorProductProgress = "prepared"
	connectorProductApplying   connectorProductProgress = "applying"
	connectorProductProgressed connectorProductProgress = "progress"
	connectorProductApplied    connectorProductProgress = "applied"
	connectorProductUndoing    connectorProductProgress = "undoing"
	connectorProductUndone     connectorProductProgress = "undone"
)

type connectorPriorProvenance struct {
	Marketplace bool   `json:"marketplace,omitempty"`
	Plugin      bool   `json:"plugin,omitempty"`
	Present     bool   `json:"present,omitempty"`
	Enabled     bool   `json:"enabled,omitempty"`
	Source      string `json:"source,omitempty"`
	Version     string `json:"version,omitempty"`
}

type connectorTreeEntryProvenance struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Body []byte `json:"body,omitempty"`
}

type connectorTreeProvenance struct {
	Present bool                           `json:"present"`
	Entries []connectorTreeEntryProvenance `json:"entries,omitempty"`
}

type connectorMutationProvenance struct {
	SchemaVersion int                      `json:"schema_version"`
	Product       string                   `json:"product"`
	Available     bool                     `json:"available"`
	Executable    string                   `json:"executable,omitempty"`
	SourceRoot    string                   `json:"source_root"`
	PayloadRoot   string                   `json:"payload_root,omitempty"`
	Scope         string                   `json:"scope,omitempty"`
	UserRoot      string                   `json:"user_root,omitempty"`
	QwenHome      string                   `json:"qwen_home,omitempty"`
	TargetVersion string                   `json:"target_version,omitempty"`
	Prior         connectorPriorProvenance `json:"prior"`
	PriorTree     connectorTreeProvenance  `json:"prior_tree"`
}

type connectorProductRecovery struct {
	Product      string                      `json:"product"`
	Prepared     bool                        `json:"prepared"`
	Progress     connectorProductProgress    `json:"progress"`
	AppliedSteps int                         `json:"applied_steps"`
	TotalSteps   int                         `json:"total_steps"`
	Provenance   connectorMutationProvenance `json:"provenance"`
}

type connectorRecoveryJournal struct {
	SchemaVersion   int                        `json:"schema_version"`
	Phase           connectorRecoveryPhase     `json:"phase"`
	Version         string                     `json:"version"`
	ContentIdentity string                     `json:"content_identity"`
	SourceRoot      string                     `json:"source_root"`
	Products        []connectorProductRecovery `json:"products"`
	UpdatedAt       int64                      `json:"updated_at"`
}

//nolint:gocyclo // The closed per-product recovery schema is validated at one trust boundary.
func (provenance connectorMutationProvenance) validate(product string) error {
	if provenance.SchemaVersion != connectorRecoverySchemaVersion || provenance.Product != product ||
		!cleanAbsoluteConnectorPath(provenance.SourceRoot) {
		return errors.New("connector recovery provenance has invalid authority")
	}
	if !provenance.Available {
		if provenance.Executable != "" || provenance.PayloadRoot != "" || provenance.Scope != "" ||
			provenance.UserRoot != "" || provenance.QwenHome != "" || provenance.TargetVersion != "" ||
			provenance.Prior != (connectorPriorProvenance{}) || provenance.PriorTree.Present || len(provenance.PriorTree.Entries) != 0 {
			return errors.New("unavailable connector recovery provenance retains mutable state")
		}
		return nil
	}
	if !cleanAbsoluteConnectorPath(provenance.Executable) || len(provenance.Executable) > 4096 ||
		strings.ContainsRune(provenance.Executable, '\x00') {
		return errors.New("connector recovery executable is invalid")
	}
	switch product {
	case "codex":
		if provenance.PayloadRoot != provenance.SourceRoot || provenance.Scope != "" || provenance.UserRoot != "" ||
			provenance.QwenHome != "" || provenance.TargetVersion != "" || provenance.PriorTree.Present ||
			len(provenance.PriorTree.Entries) != 0 || provenance.Prior.Present || provenance.Prior.Enabled ||
			provenance.Prior.Version != "" || !validMarketplacePrior(provenance.Prior) {
			return errors.New("codex connector recovery provenance is invalid")
		}
	case "claude":
		if provenance.PayloadRoot != filepath.Join(provenance.SourceRoot, "claude") || provenance.Scope != "user" ||
			provenance.UserRoot != "" || provenance.QwenHome != "" || provenance.TargetVersion != "" ||
			provenance.PriorTree.Present || len(provenance.PriorTree.Entries) != 0 || provenance.Prior.Present ||
			provenance.Prior.Enabled || provenance.Prior.Version != "" || !validMarketplacePrior(provenance.Prior) {
			return errors.New("claude connector recovery provenance is invalid")
		}
	case "grok":
		if provenance.PayloadRoot != filepath.Join(provenance.SourceRoot, "grok") || provenance.Scope != "" ||
			!cleanAbsoluteConnectorPath(provenance.UserRoot) || filepath.Base(provenance.UserRoot) != "agent-sessions" ||
			provenance.QwenHome != "" || provenance.TargetVersion != "" || provenance.Prior.Marketplace ||
			provenance.Prior.Plugin || provenance.Prior.Source != "" || provenance.Prior.Version != "" ||
			(provenance.Prior.Present && !provenance.Prior.Enabled) ||
			(!provenance.Prior.Present && provenance.Prior.Enabled) {
			return errors.New("grok connector recovery provenance is invalid")
		}
		if err := provenance.PriorTree.validate(); err != nil {
			return fmt.Errorf("grok connector recovery snapshot: %w", err)
		}
	case "qwen":
		if provenance.PayloadRoot != filepath.Join(provenance.SourceRoot, "qwen") || provenance.Scope != "" ||
			provenance.UserRoot != "" || !cleanAbsoluteConnectorPath(provenance.QwenHome) ||
			provenance.TargetVersion == "" || provenance.PriorTree.Present || len(provenance.PriorTree.Entries) != 0 ||
			provenance.Prior.Marketplace || provenance.Prior.Plugin ||
			(provenance.Prior.Present && (!provenance.Prior.Enabled || !cleanAbsoluteConnectorPath(provenance.Prior.Source) || provenance.Prior.Version == "")) ||
			(!provenance.Prior.Present && provenance.Prior != (connectorPriorProvenance{})) {
			return errors.New("qwen connector recovery provenance is invalid")
		}
	default:
		return errors.New("connector recovery provenance names an unknown product")
	}
	return nil
}

func validMarketplacePrior(prior connectorPriorProvenance) bool {
	if prior.Present || prior.Enabled || prior.Version != "" {
		return false
	}
	if prior.Marketplace {
		return cleanAbsoluteConnectorPath(prior.Source)
	}
	return prior.Source == "" && !prior.Plugin
}

func cleanAbsoluteConnectorPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

//nolint:gocyclo // The bounded tree snapshot validates every entry and aggregate invariant together.
func (tree connectorTreeProvenance) validate() error {
	if !tree.Present {
		if len(tree.Entries) != 0 {
			return errors.New("absent tree retains entries")
		}
		return nil
	}
	if len(tree.Entries) == 0 || len(tree.Entries) > 2048 {
		return errors.New("tree entry count is invalid")
	}
	seen := make(map[string]struct{}, len(tree.Entries))
	total, roots := 0, 0
	for _, entry := range tree.Entries {
		relative := filepath.Clean(filepath.FromSlash(entry.Path))
		if entry.Path == "." {
			roots++
		} else if entry.Path == "" || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.ToSlash(relative) != entry.Path {
			return errors.New("tree entry path is not canonical")
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return errors.New("tree repeats an entry")
		}
		seen[entry.Path] = struct{}{}
		mode := os.FileMode(entry.Mode)
		if mode&os.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) || mode.Perm() == 0 {
			return errors.New("tree entry mode is invalid")
		}
		if mode.IsDir() && len(entry.Body) != 0 {
			return errors.New("tree directory retains file content")
		}
		if len(entry.Body) > 2<<20 || total > 16<<20-len(entry.Body) {
			return errors.New("tree content exceeds rollback bound")
		}
		total += len(entry.Body)
	}
	if roots != 1 {
		return errors.New("tree lacks its exact root")
	}
	return nil
}

func snapshotConnectorTreeProvenance(snapshot connectorTreeSnapshot) connectorTreeProvenance {
	result := connectorTreeProvenance{Present: snapshot.present, Entries: make([]connectorTreeEntryProvenance, 0, len(snapshot.entries))}
	for _, entry := range snapshot.entries {
		result.Entries = append(result.Entries, connectorTreeEntryProvenance{
			Path: filepath.ToSlash(entry.path), Mode: uint32(entry.mode), Body: append([]byte(nil), entry.body...),
		})
	}
	return result
}

func (tree connectorTreeProvenance) snapshot() connectorTreeSnapshot {
	result := connectorTreeSnapshot{present: tree.Present, entries: make([]connectorTreeEntry, 0, len(tree.Entries))}
	for _, entry := range tree.Entries {
		result.entries = append(result.entries, connectorTreeEntry{
			path: filepath.FromSlash(entry.Path), mode: os.FileMode(entry.Mode), body: append([]byte(nil), entry.Body...),
		})
	}
	return result
}

//nolint:gocyclo // Journal validation keeps every phase/progress invariant fail-closed in one boundary.
func (record connectorRecoveryJournal) validate() error {
	if record.SchemaVersion != connectorRecoverySchemaVersion || record.Version == "" || record.ContentIdentity == "" ||
		!cleanAbsoluteConnectorPath(record.SourceRoot) || record.UpdatedAt <= 0 {
		return errors.New("connector recovery journal has invalid authority")
	}
	switch record.Phase {
	case connectorRecoveryPreparing, connectorRecoveryPrepared, connectorRecoveryCommitting,
		connectorRecoveryRollingBack, connectorRecoveryCommitted, connectorRecoveryRolledBack:
	default:
		return errors.New("connector recovery journal has unsupported phase")
	}
	catalog := productcatalog.Catalog().Products
	requiresCompleteInventory := record.Phase == connectorRecoveryPrepared || record.Phase == connectorRecoveryCommitting ||
		record.Phase == connectorRecoveryCommitted
	if len(record.Products) > len(catalog) || (requiresCompleteInventory && len(record.Products) != len(catalog)) {
		return errors.New("connector recovery journal has incomplete product inventory")
	}
	for index := range record.Products {
		product := record.Products[index]
		if !product.Prepared || product.Product != catalog[index].ID || product.TotalSteps < 0 || product.TotalSteps > 32 ||
			product.AppliedSteps < 0 || product.AppliedSteps > product.TotalSteps ||
			product.Provenance.SourceRoot != record.SourceRoot {
			return errors.New("connector recovery journal has invalid product progress")
		}
		if err := product.Provenance.validate(product.Product); err != nil {
			return err
		}
		switch product.Progress {
		case connectorProductPrepared:
			if product.AppliedSteps != 0 {
				return errors.New("prepared connector retains applied steps")
			}
		case connectorProductApplying:
			if product.AppliedSteps >= product.TotalSteps {
				return errors.New("applying connector has no pending step")
			}
		case connectorProductProgressed:
			if product.AppliedSteps <= 0 || product.AppliedSteps >= product.TotalSteps {
				return errors.New("connector progress is not intermediate")
			}
		case connectorProductApplied:
			if product.AppliedSteps != product.TotalSteps {
				return errors.New("applied connector has incomplete steps")
			}
		case connectorProductUndoing:
			if product.AppliedSteps <= 0 {
				return errors.New("undoing connector has no applied step")
			}
		case connectorProductUndone:
			if product.AppliedSteps != 0 {
				return errors.New("undone connector retains applied steps")
			}
		default:
			return errors.New("connector recovery journal has unsupported product progress")
		}
	}
	if record.Phase == connectorRecoveryCommitted {
		for _, product := range record.Products {
			if product.Progress != connectorProductApplied {
				return errors.New("committed connector journal is not fully applied")
			}
		}
	}
	if record.Phase == connectorRecoveryRolledBack {
		for _, product := range record.Products {
			if product.Progress != connectorProductUndone {
				return errors.New("rolled-back connector journal is not fully undone")
			}
		}
	}
	return nil
}

func saveConnectorRecoveryJournal(path string, record connectorRecoveryJournal) error {
	if !cleanAbsoluteConnectorPath(path) || filepath.Base(path) != "connectors.json" {
		return errors.New("connector recovery journal path is not canonical")
	}
	record.UpdatedAt = time.Now().UnixMilli()
	if err := record.validate(); err != nil {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil || int64(len(body)) > maxConnectorRecoveryBytes {
		return errors.New("connector recovery journal exceeds its bound")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".connector-recovery-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncHostSurfaceDirectory(directory)
}

func loadConnectorRecoveryJournal(path string) (connectorRecoveryJournal, error) {
	body, err := readDaemonBoundedRegularFile(path, maxConnectorRecoveryBytes)
	if err != nil {
		return connectorRecoveryJournal{}, err
	}
	var record connectorRecoveryJournal
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || decoder.Decode(new(any)) != io.EOF {
		return connectorRecoveryJournal{}, errors.New("connector recovery journal is malformed")
	}
	if err := record.validate(); err != nil {
		return connectorRecoveryJournal{}, err
	}
	return record, nil
}

func reconstructConnectorMutation(
	runner ConnectorCommandRunner,
	provenance connectorMutationProvenance,
) (durableConnectorMutation, error) {
	if err := provenance.validate(provenance.Product); err != nil {
		return nil, err
	}
	if runner == nil {
		runner = osConnectorCommandRunner{}
	}
	if !provenance.Available {
		return unavailableConnectorMutation(provenance.Product, provenance.SourceRoot), nil
	}
	switch provenance.Product {
	case "codex":
		return newCodexConnectorMutation(runner, provenance.Executable, provenance.SourceRoot, codexConnectorState{
			marketplace: provenance.Prior.Marketplace,
			plugin:      provenance.Prior.Plugin,
			source:      provenance.Prior.Source,
		}), nil
	case "claude":
		return newClaudeConnectorMutation(runner, provenance.Executable, provenance.Scope, provenance.SourceRoot, claudeConnectorState{
			marketplace: provenance.Prior.Marketplace,
			plugin:      provenance.Prior.Plugin,
			source:      provenance.Prior.Source,
		}), nil
	case "grok":
		return newGrokConnectorMutation(
			runner, provenance.Executable, provenance.SourceRoot, provenance.PayloadRoot, provenance.UserRoot,
			grokConnectorState{present: provenance.Prior.Present, enabled: provenance.Prior.Enabled},
			provenance.PriorTree.snapshot(),
		), nil
	case "qwen":
		return newQwenConnectorMutation(
			runner, provenance.Executable, provenance.SourceRoot, provenance.PayloadRoot, provenance.QwenHome,
			provenance.TargetVersion, qwenConnectorState{
				present: provenance.Prior.Present, enabled: provenance.Prior.Enabled,
				source: provenance.Prior.Source, version: provenance.Prior.Version,
			},
		), nil
	default:
		return nil, errors.New("connector recovery provenance names an unknown product")
	}
}

//nolint:gocyclo // Preparation persists each closed-catalog product before advancing to the next.
func (hooks *HostInstallHooks) prepareDurable(
	ctx context.Context,
	request releaseinstall.InstallRequest,
) error {
	if len(hooks.drivers) != len(productcatalog.Catalog().Products) {
		return errors.New("durable connector prepare requires configured native drivers")
	}
	if existing, err := loadConnectorRecoveryJournal(hooks.journalPath); err == nil {
		if existing.Phase != connectorRecoveryCommitted && existing.Phase != connectorRecoveryRolledBack {
			return fmt.Errorf("connector transaction requires recovery from phase %q", existing.Phase)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	record := connectorRecoveryJournal{
		SchemaVersion: connectorRecoverySchemaVersion,
		Phase:         connectorRecoveryPreparing, Version: request.Version,
		ContentIdentity: request.ContentIdentity, SourceRoot: request.SourceRoot,
	}
	if err := saveConnectorRecoveryJournal(hooks.journalPath, record); err != nil {
		return err
	}
	for _, product := range productcatalog.Catalog().Products {
		mutation, err := hooks.drivers[product.ID].Prepare(ctx, ConnectorRequest{
			Product: product.ID, SourceRoot: request.SourceRoot, Descriptor: product.Connector,
		})
		if err != nil {
			return errors.Join(fmt.Errorf("prepare %s connector: %w", product.ID, err), hooks.rollbackDurable(ctx))
		}
		durable, ok := mutation.(durableConnectorMutation)
		if !ok || mutation == nil {
			return errors.Join(
				fmt.Errorf("prepare %s connector returned no durable recovery provenance", product.ID),
				hooks.rollbackDurable(ctx),
			)
		}
		provenance := durable.recoveryProvenance()
		if provenance.Product != product.ID || provenance.SourceRoot != request.SourceRoot {
			return errors.Join(
				fmt.Errorf("prepare %s connector returned mismatched recovery provenance", product.ID),
				hooks.rollbackDurable(ctx),
			)
		}
		if err := provenance.validate(product.ID); err != nil {
			return errors.Join(err, hooks.rollbackDurable(ctx))
		}
		record.Products = append(record.Products, connectorProductRecovery{
			Product: product.ID, Prepared: true, Progress: connectorProductPrepared,
			TotalSteps: durable.recoveryStepCount(), Provenance: provenance,
		})
		if err := saveConnectorRecoveryJournal(hooks.journalPath, record); err != nil {
			return errors.Join(err, hooks.rollbackDurable(ctx))
		}
	}
	record.Phase = connectorRecoveryPrepared
	if err := saveConnectorRecoveryJournal(hooks.journalPath, record); err != nil {
		return errors.Join(err, hooks.rollbackDurable(ctx))
	}
	if hooks.crashCheckpoint != nil {
		hooks.crashCheckpoint("prepared", "")
	}
	return nil
}

//nolint:gocyclo // Commit resumes each durable per-product state without an implicit completion path.
func (hooks *HostInstallHooks) commitDurable(ctx context.Context) error {
	record, err := loadConnectorRecoveryJournal(hooks.journalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("durable connector commit has no prepared provenance")
		}
		return err
	}
	switch record.Phase {
	case connectorRecoveryCommitted:
		return nil
	case connectorRecoveryPrepared, connectorRecoveryCommitting:
	case connectorRecoveryRolledBack, connectorRecoveryRollingBack:
		return fmt.Errorf("cannot commit connector transaction from phase %q", record.Phase)
	case connectorRecoveryPreparing:
		return errors.New("cannot commit an incompletely prepared connector transaction")
	default:
		return errors.New("connector transaction has unsupported commit phase")
	}
	record.Phase = connectorRecoveryCommitting
	if err := saveConnectorRecoveryJournal(hooks.journalPath, record); err != nil {
		return err
	}
	for index := range record.Products {
		product := &record.Products[index]
		if product.Progress == connectorProductApplied {
			continue
		}
		if product.Progress == connectorProductUndoing || product.Progress == connectorProductUndone {
			return fmt.Errorf("cannot resume %s connector commit from progress %q", product.Product, product.Progress)
		}
		mutation, err := hooks.reconstructDurableMutation(*product)
		if err != nil {
			return fmt.Errorf("reconstruct %s connector commit: %w", product.Product, err)
		}
		if err := mutation.resumeRecovery(product.AppliedSteps, hooks.productCheckpoint(&record, index)); err != nil {
			return err
		}
		if err := mutation.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s connector: %w", product.Product, err)
		}
		if product.Progress != connectorProductApplied {
			product.AppliedSteps = product.TotalSteps
			product.Progress = connectorProductApplied
			if err := saveConnectorRecoveryJournal(hooks.journalPath, record); err != nil {
				return err
			}
			if hooks.crashCheckpoint != nil {
				hooks.crashCheckpoint("applied", product.Product)
			}
		}
	}
	record.Phase = connectorRecoveryCommitted
	return saveConnectorRecoveryJournal(hooks.journalPath, record)
}

func (hooks *HostInstallHooks) retargetDurableSource(release releaseinstall.InstalledRelease) error {
	if release.Role != releaseinstall.RoleHost || !cleanAbsoluteConnectorPath(release.Root) ||
		release.Version == "" || release.ContentIdentity == "" {
		return errors.New("installed connector release identity is invalid")
	}
	record, err := loadConnectorRecoveryJournal(hooks.journalPath)
	if err != nil {
		return err
	}
	if record.Version != release.Version || record.ContentIdentity != release.ContentIdentity {
		return errors.New("prepared connector transaction does not match the installed release")
	}
	if record.SourceRoot == release.Root {
		return nil
	}
	if record.Phase != connectorRecoveryPrepared {
		return fmt.Errorf("cannot retarget connector transaction from phase %q", record.Phase)
	}
	record.SourceRoot = release.Root
	for index := range record.Products {
		if err := retargetConnectorProduct(&record.Products[index], release.Root); err != nil {
			return err
		}
	}
	return saveConnectorRecoveryJournal(hooks.journalPath, record)
}

func retargetConnectorProduct(product *connectorProductRecovery, releaseRoot string) error {
	if product.AppliedSteps != 0 || product.Progress != connectorProductPrepared {
		return fmt.Errorf("cannot retarget applied %s connector", product.Product)
	}
	product.Provenance.SourceRoot = releaseRoot
	if product.Provenance.Available {
		payloadRoot, err := installedConnectorPayloadRoot(product.Product, releaseRoot)
		if err != nil {
			return err
		}
		product.Provenance.PayloadRoot = payloadRoot
	}
	return product.Provenance.validate(product.Product)
}

func installedConnectorPayloadRoot(product, releaseRoot string) (string, error) {
	switch product {
	case "codex":
		return releaseRoot, nil
	case "claude", "grok", "qwen":
		return filepath.Join(releaseRoot, product), nil
	default:
		return "", errors.New("connector transaction names an unknown product")
	}
}

func (hooks *HostInstallHooks) rollbackDurable(ctx context.Context) error {
	record, err := loadConnectorRecoveryJournal(hooks.journalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if record.Phase == connectorRecoveryRolledBack {
		return nil
	}
	record.Phase = connectorRecoveryRollingBack
	if err := saveConnectorRecoveryJournal(hooks.journalPath, record); err != nil {
		return err
	}
	for index := len(record.Products) - 1; index >= 0; index-- {
		product := &record.Products[index]
		if product.Progress == connectorProductUndone {
			continue
		}
		mutation, err := hooks.reconstructDurableMutation(*product)
		if err != nil {
			return fmt.Errorf("reconstruct %s connector rollback: %w", product.Product, err)
		}
		appliedSteps := product.AppliedSteps
		if product.Progress == connectorProductApplying {
			// The apply intent was durable before the native call. Treat the
			// uncertain step as applied; every native undo is repeat-safe and
			// restores the exact captured prior state if the call took effect.
			appliedSteps++
		}
		if err := mutation.resumeRecovery(appliedSteps, hooks.productCheckpoint(&record, index)); err != nil {
			return err
		}
		if err := mutation.Rollback(ctx); err != nil {
			return fmt.Errorf("rollback %s connector: %w", product.Product, err)
		}
		if product.Progress != connectorProductUndone {
			product.AppliedSteps = 0
			product.Progress = connectorProductUndone
			if err := saveConnectorRecoveryJournal(hooks.journalPath, record); err != nil {
				return err
			}
		}
	}
	record.Phase = connectorRecoveryRolledBack
	return saveConnectorRecoveryJournal(hooks.journalPath, record)
}

func (hooks *HostInstallHooks) reconstructDurableMutation(
	product connectorProductRecovery,
) (durableConnectorMutation, error) {
	mutation, err := reconstructConnectorMutation(hooks.recoveryRunner, product.Provenance)
	if err != nil {
		return nil, err
	}
	if mutation.recoveryStepCount() != product.TotalSteps {
		return nil, errors.New("connector recovery step inventory changed")
	}
	return mutation, nil
}

func (hooks *HostInstallHooks) productCheckpoint(
	record *connectorRecoveryJournal,
	index int,
) connectorMutationCheckpoint {
	return func(appliedSteps int, progress connectorProductProgress) error {
		product := &record.Products[index]
		product.AppliedSteps = appliedSteps
		product.Progress = progress
		if err := saveConnectorRecoveryJournal(hooks.journalPath, *record); err != nil {
			return err
		}
		if hooks.crashCheckpoint != nil && progress == connectorProductApplied {
			hooks.crashCheckpoint("applied", product.Product)
		}
		return nil
	}
}
