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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/servicecontrol"
)

const maxLifecyclePlanBytes = 1024 * 1024

// RemovalBlocker is one exact managed attachment or lane that prevents a
// normal removal from beginning.
type RemovalBlocker struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	State string `json:"state"`
}

// RemovalTarget identifies one Agent Sessions-owned lifecycle artifact.
// Paths are absolute, clean and re-attested by the operation-specific remover
// immediately before mutation.
type RemovalTarget struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// RemovalPlan is the exact online normal-removal inventory.
type RemovalPlan struct {
	Role       string           `json:"role"`
	Revision   string           `json:"revision"`
	Blockers   []RemovalBlocker `json:"blockers"`
	Targets    []RemovalTarget  `json:"targets"`
	Preserved  []string         `json:"preserved"`
	Exclusions []string         `json:"exclusions"`
}

// PurgePlan is an offline, state-revision-bound deletion plan.
type PurgePlan struct {
	Role          string          `json:"role"`
	PlanRevision  string          `json:"plan_revision"`
	StateRevision string          `json:"state_revision"`
	Targets       []RemovalTarget `json:"targets"`
	Exclusions    []string        `json:"exclusions"`
}

// RemovalHooks supplies supported lifecycle operations. Callers retain exact
// service-manager, connector and filesystem identity checks at their boundary.
type RemovalHooks struct {
	StopService      func(context.Context) error
	RemoveConnectors func(context.Context) error
	RemoveTarget     func(context.Context, RemovalTarget) error
}

// RemovalResult records the committed deletion prefix and retryable debt.
type RemovalResult struct {
	Role         string       `json:"role"`
	PlanRevision string       `json:"plan_revision"`
	Deleted      []string     `json:"deleted"`
	Debt         []DebtRecord `json:"debt"`
}

// RemovalInspection is the stable metadata-only online host removal plan.
type RemovalInspection struct {
	Role      string           `json:"role"`
	Revision  string           `json:"revision"`
	Blockers  []RemovalBlocker `json:"blockers"`
	Targets   []RemovalTarget  `json:"targets"`
	Preserved []string         `json:"preserved"`
}

// PurgeInspection is the stable public projection of a private exact plan.
type PurgeInspection struct {
	Role         string   `json:"role"`
	PlanRevision string   `json:"plan_revision"`
	Targets      []string `json:"targets"`
	Exclusions   []string `json:"exclusions"`
}

// PurgeApplyResult is the stable public committed purge projection.
type PurgeApplyResult struct {
	Role         string       `json:"role"`
	PlanRevision string       `json:"plan_revision"`
	Deleted      []string     `json:"deleted"`
	Debt         []DebtRecord `json:"debt"`
}

type lifecycleRefusalError struct{ cause error }

func (failure *lifecycleRefusalError) Error() string { return failure.cause.Error() }
func (failure *lifecycleRefusalError) Unwrap() error { return failure.cause }

// ExitCode identifies the stable refused-operation class.
func (*lifecycleRefusalError) ExitCode() int { return 4 }

// ApplyRemoval refuses all blockers before the first mutation, then performs
// the supported service, connector and exact target sequence.
func ApplyRemoval(ctx context.Context, plan RemovalPlan, hooks RemovalHooks) (RemovalResult, error) {
	result := RemovalResult{Role: plan.Role, PlanRevision: plan.Revision, Deleted: []string{}, Debt: []DebtRecord{}}
	if err := validateRemovalPlan(plan); err != nil {
		return result, err
	}
	if len(plan.Blockers) != 0 {
		blockers := append([]RemovalBlocker(nil), plan.Blockers...)
		sort.Slice(blockers, func(left, right int) bool {
			if blockers[left].Kind == blockers[right].Kind {
				return blockers[left].ID < blockers[right].ID
			}
			return blockers[left].Kind < blockers[right].Kind
		})
		labels := make([]string, 0, len(blockers))
		for _, blocker := range blockers {
			labels = append(labels, blocker.Kind+":"+blocker.ID+"("+blocker.State+")")
		}
		return result, fmt.Errorf("refuse removal while managed blockers remain: %s", strings.Join(labels, ", "))
	}
	if hooks.StopService == nil || hooks.RemoveTarget == nil {
		return result, errors.New("removal requires service-stop and exact-target hooks")
	}
	if err := hooks.StopService(ctx); err != nil {
		return result, fmt.Errorf("stop service before removal: %w", err)
	}
	if hooks.RemoveConnectors != nil {
		if err := hooks.RemoveConnectors(ctx); err != nil {
			return result, fmt.Errorf("remove vendor connectors: %w", err)
		}
	}
	for _, target := range plan.Targets {
		if err := hooks.RemoveTarget(ctx, target); err != nil {
			result.Debt = append(result.Debt, removalDebt("remove", target, plan.Revision, err))
			return result, fmt.Errorf("remove %s %q: %w", target.Kind, target.Path, err)
		}
		result.Deleted = append(result.Deleted, target.Path)
	}
	return result, nil
}

// ApplyPurge applies only the exact current revision of a validated offline
// plan. The caller-supplied remover must re-attest type, UID, root containment
// and identity immediately before each deletion.
func ApplyPurge(
	ctx context.Context,
	plan PurgePlan,
	currentStateRevision string,
	remove func(context.Context, RemovalTarget) error,
) (RemovalResult, error) {
	result := RemovalResult{Role: plan.Role, PlanRevision: plan.PlanRevision, Deleted: []string{}, Debt: []DebtRecord{}}
	if err := validatePurgePlan(plan); err != nil {
		return result, err
	}
	if currentStateRevision != plan.StateRevision {
		return result, fmt.Errorf("purge state revision changed: current=%q plan=%q", currentStateRevision, plan.StateRevision)
	}
	if remove == nil {
		return result, errors.New("purge requires an exact-target remover")
	}
	for _, target := range plan.Targets {
		if err := remove(ctx, target); err != nil {
			result.Debt = append(result.Debt, removalDebt("purge", target, plan.PlanRevision, err))
			return result, fmt.Errorf("purge %s %q: %w", target.Kind, target.Path, err)
		}
		result.Deleted = append(result.Deleted, target.Path)
	}
	return result, nil
}

func validateRemovalPlan(plan RemovalPlan) error {
	if err := validateRoleAndRevision(plan.Role, plan.Revision); err != nil {
		return err
	}
	if err := validateTargets(plan.Targets, append(append([]string(nil), plan.Preserved...), plan.Exclusions...)); err != nil {
		return err
	}
	for _, blocker := range plan.Blockers {
		if strings.TrimSpace(blocker.Kind) == "" || strings.TrimSpace(blocker.ID) == "" || strings.TrimSpace(blocker.State) == "" {
			return errors.New("removal blocker has incomplete exact identity")
		}
	}
	return nil
}

func validatePurgePlan(plan PurgePlan) error {
	if err := validateRoleAndRevision(plan.Role, plan.PlanRevision); err != nil {
		return err
	}
	if strings.TrimSpace(plan.StateRevision) == "" {
		return errors.New("purge plan requires a state revision")
	}
	return validateTargets(plan.Targets, plan.Exclusions)
}

func validateRoleAndRevision(role, revision string) error {
	if role != "host" && role != "hub" {
		return fmt.Errorf("unsupported lifecycle role %q", role)
	}
	if strings.TrimSpace(revision) == "" {
		return errors.New("lifecycle plan requires a revision")
	}
	return nil
}

func validateTargets(targets []RemovalTarget, exclusions []string) error {
	for _, exclusion := range exclusions {
		if !cleanAbsolutePath(exclusion) {
			return fmt.Errorf("excluded path %q is not clean and absolute", exclusion)
		}
	}
	for _, target := range targets {
		if strings.TrimSpace(target.Kind) == "" || !cleanAbsolutePath(target.Path) {
			return fmt.Errorf("removal target has invalid kind or path %q", target.Path)
		}
		for _, exclusion := range exclusions {
			if pathWithin(target.Path, exclusion) {
				return fmt.Errorf("target %q is within excluded path %q", target.Path, exclusion)
			}
		}
	}
	return nil
}

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func removalDebt(operation string, target RemovalTarget, revision string, cause error) DebtRecord {
	now := time.Now().UnixMilli()
	id := strings.NewReplacer(string(filepath.Separator), "_", " ", "_", ":", "_").Replace(target.Kind + "-" + filepath.Base(target.Path))
	if !durableRecordID.MatchString(id) {
		id = "lifecycle-target"
	}
	return DebtRecord{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, CreatedAt: now, UpdatedAt: now},
		DebtID:       id, Operation: operation, ResourceKind: target.Kind, ResourceIdentity: target.Path,
		ExpectedRevision: revision, CauseCode: operation + "_incomplete", CauseDetail: boundedRemovalCause(cause),
		RetryPredicate: "target identity and plan revision still match", ProhibitedScope: "vendor and other-role state",
	}
}

func boundedRemovalCause(err error) string {
	if err == nil {
		return ""
	}
	detail := err.Error()
	if len(detail) > 256 {
		return detail[:256]
	}
	return detail
}

// RemovalInspection snapshots the exact currently known blocker and lifecycle
// target inventory without mutating service lifetime.
func (runtime *Runtime) RemovalInspection() RemovalInspection {
	runtime.mu.RLock()
	revision := fmt.Sprintf("generation:%d/state:%d", runtime.generation, runtime.stateRevision)
	paths := runtime.options.Paths
	runtime.mu.RUnlock()
	prefix := defaultInstallPrefix()
	return RemovalInspection{
		Role: "host", Revision: revision, Blockers: []RemovalBlocker{},
		Targets:   hostRemovalTargets(prefix, paths),
		Preserved: []string{paths.ConfigurationRoot, paths.StateRoot},
	}
}

// RunHostRemoveCLI performs the host-only normal removal selected by Make.
// It requires one fresh online blocker inventory before the shared role engine
// stops or mutates anything.
func RunHostRemoveCLI(ctx context.Context, args []string) error {
	values, err := parseHostRemovalOptions(args)
	if err != nil {
		return err
	}
	if values["--role"] != "host" {
		return errors.New("host remover accepts only --role host")
	}
	prefix := values["--prefix"]
	if !cleanAbsolutePath(prefix) {
		return errors.New("host removal prefix must be a clean absolute non-root path")
	}
	body, err := QueryAdmin(ctx, "remove.inspect")
	if err != nil {
		return err
	}
	inspection, err := decodeRemovalInspection(body)
	if err != nil {
		return err
	}
	if len(inspection.Blockers) != 0 {
		plan := RemovalPlan{Role: inspection.Role, Revision: inspection.Revision, Blockers: inspection.Blockers}
		_, refusal := ApplyRemoval(ctx, plan, RemovalHooks{})
		return &lifecycleRefusalError{cause: refusal}
	}
	paths, err := ResolveProductionPaths()
	if err != nil {
		return err
	}
	engine, err := newHostRemovalEngine(prefix, paths, values)
	if err != nil {
		return err
	}
	return engine.Remove(ctx)
}

// RunHostPurgeInspectCLI creates one offline exact plan and writes it to the
// operator-selected regular file without starting or contacting the daemon.
func RunHostPurgeInspectCLI(ctx context.Context, planPath string) (PurgeInspection, error) {
	engine, paths, err := newHostPurgeEngine(defaultInstallPrefix())
	if err != nil {
		return PurgeInspection{}, err
	}
	if err := requireHostRemoved(paths, engine.layout.CurrentSelection); err != nil {
		return PurgeInspection{}, err
	}
	plan, err := engine.engine.PlanPurge(ctx)
	if err != nil {
		return PurgeInspection{}, err
	}
	if err := writeHostPurgePlan(planPath, plan); err != nil {
		return PurgeInspection{}, err
	}
	return purgeInspection(plan), nil
}

// RunHostPurgeApplyCLI applies only a still-current exact offline plan.
func RunHostPurgeApplyCLI(ctx context.Context, planPath string) (PurgeApplyResult, error) {
	engine, paths, err := newHostPurgeEngine(defaultInstallPrefix())
	if err != nil {
		return PurgeApplyResult{}, err
	}
	if err := requireHostRemoved(paths, engine.layout.CurrentSelection); err != nil {
		return PurgeApplyResult{}, err
	}
	plan, err := readHostPurgePlan(planPath)
	if err != nil {
		return PurgeApplyResult{}, err
	}
	if err := engine.engine.ApplyPurge(ctx, plan); err != nil {
		if errors.Is(err, releaseinstall.ErrRevisionConflict) {
			return PurgeApplyResult{}, &lifecycleRefusalError{cause: err}
		}
		return PurgeApplyResult{}, err
	}
	return PurgeApplyResult{
		Role: string(plan.Role), PlanRevision: plan.Revision,
		Deleted: append([]string(nil), plan.Targets...), Debt: []DebtRecord{},
	}, nil
}

type hostPurgeEngine struct {
	engine *releaseinstall.Engine
	layout releaseinstall.RoleLayout
}

func newHostRemovalEngine(prefix string, paths ProductionPaths, values map[string]string) (*releaseinstall.Engine, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	drivers, err := NewNativeConnectorDrivers(NativeConnectorOptions{
		CodexExecutable: values["--codex"], ClaudeExecutable: values["--claude"],
		GrokExecutable: values["--grok"], QwenExecutable: values["--qwen"],
		GrokUserPluginRoot: filepath.Join(home, ".grok", "plugins", "agent-sessions"),
	})
	if err != nil {
		return nil, err
	}
	connectors, err := NewHostInstallHooks(drivers)
	if err != nil {
		return nil, err
	}
	lifecycle, err := NewHostInstallLifecycle(
		connectors,
		MigrationInspector(func(context.Context, releaseinstall.InstallRequest) error { return nil }),
		func(context.Context, releaseinstall.InstalledRelease) error { return nil },
	)
	if err != nil {
		return nil, err
	}
	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHost)
	if err != nil {
		return nil, err
	}
	service, err := newInstalledHostService(prefix)
	if err != nil {
		return nil, err
	}
	hooks := &hostInstallRoleHooks{
		lifecycle: lifecycle, prefix: prefix, stateRoot: paths.StateRoot, runtimeEndpoint: paths.ControlEndpoint,
	}
	return releaseinstall.NewEngine(releaseinstall.EngineOptions{
		Layout: layout, Service: service, Hooks: hooks,
		PurgeTargets:    []string{paths.ConfigurationRoot, paths.StateRoot},
		PurgeExclusions: hostVendorExclusions(home),
	})
}

func newHostPurgeEngine(prefix string) (hostPurgeEngine, ProductionPaths, error) {
	paths, err := ResolveProductionPaths()
	if err != nil {
		return hostPurgeEngine{}, ProductionPaths{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return hostPurgeEngine{}, ProductionPaths{}, err
	}
	layout, err := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHost)
	if err != nil {
		return hostPurgeEngine{}, ProductionPaths{}, err
	}
	service, err := newInstalledHostService(prefix)
	if err != nil {
		return hostPurgeEngine{}, ProductionPaths{}, err
	}
	engine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{
		Layout: layout, Service: service, Hooks: noRoleHooks{},
		PurgeTargets:    []string{paths.ConfigurationRoot, paths.StateRoot},
		PurgeExclusions: hostVendorExclusions(home),
	})
	return hostPurgeEngine{engine: engine, layout: layout}, paths, err
}

type noRoleHooks struct{}

// Prepare is an intentional offline purge no-op.
func (noRoleHooks) Prepare(context.Context, releaseinstall.InstallRequest) error { return nil }

// Ready is an intentional offline purge no-op.
func (noRoleHooks) Ready(context.Context, releaseinstall.InstalledRelease) error { return nil }

// Commit is an intentional offline purge no-op.
func (noRoleHooks) Commit(context.Context) error { return nil }

// Rollback is an intentional offline purge no-op.
func (noRoleHooks) Rollback(context.Context) error { return nil }

// Remove is an intentional offline purge no-op.
func (noRoleHooks) Remove(context.Context) error { return nil }

func parseHostRemovalOptions(args []string) (map[string]string, error) {
	allowed := map[string]bool{
		"--role": true, "--prefix": true, "--codex": true, "--claude": true, "--grok": true, "--qwen": true,
	}
	values := map[string]string{}
	for len(args) != 0 {
		if len(args) < 2 || !allowed[args[0]] || args[1] == "" || values[args[0]] != "" {
			return nil, errors.New("usage: lifecycle remove --role host --prefix PREFIX [--codex PATH --claude PATH --grok PATH --qwen PATH]")
		}
		values[args[0]], args = args[1], args[2:]
	}
	for _, required := range []string{"--role", "--prefix"} {
		if values[required] == "" {
			return nil, fmt.Errorf("host removal requires %s", required)
		}
	}
	return values, nil
}

func decodeRemovalInspection(body []byte) (RemovalInspection, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var inspection RemovalInspection
	if err := decoder.Decode(&inspection); err != nil {
		return RemovalInspection{}, fmt.Errorf("decode host removal inspection: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF || inspection.Role != "host" || inspection.Revision == "" {
		return RemovalInspection{}, errors.New("host removal inspection has invalid authority")
	}
	plan := RemovalPlan{
		Role: inspection.Role, Revision: inspection.Revision, Blockers: inspection.Blockers,
		Targets: inspection.Targets, Preserved: inspection.Preserved,
	}
	if err := validateRemovalPlan(plan); err != nil {
		return RemovalInspection{}, err
	}
	return inspection, nil
}

func purgeInspection(plan releaseinstall.PurgePlan) PurgeInspection {
	return PurgeInspection{
		Role: string(plan.Role), PlanRevision: plan.Revision,
		Targets: append([]string(nil), plan.Targets...), Exclusions: append([]string(nil), plan.Exclusions...),
	}
}

//nolint:gocyclo // Exact path validation and one durable atomic file replacement stay visible together.
func writeHostPurgePlan(path string, plan releaseinstall.PurgePlan) error {
	if !cleanAbsolutePath(path) {
		return errors.New("purge plan path must be clean, absolute, and non-root")
	}
	for _, target := range plan.Targets {
		if pathWithin(path, target) {
			return errors.New("purge plan file must be outside every purge target")
		}
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if len(body) > maxLifecyclePlanBytes {
		return errors.New("purge plan exceeds its bound")
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxLifecyclePlanBytes {
			return errors.New("existing purge plan changed filesystem type or bound")
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".agent-sessions-purge-*")
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
	return os.Rename(temporaryPath, path)
}

func readHostPurgePlan(path string) (releaseinstall.PurgePlan, error) {
	if !cleanAbsolutePath(path) {
		return releaseinstall.PurgePlan{}, errors.New("purge plan path must be clean, absolute, and non-root")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxLifecyclePlanBytes {
		return releaseinstall.PurgePlan{}, errors.New("purge plan is not a bounded real regular file")
	}
	body, err := os.ReadFile(path) //nolint:gosec // Explicit bounded operator-selected lifecycle plan.
	if err != nil {
		return releaseinstall.PurgePlan{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var plan releaseinstall.PurgePlan
	if err := decoder.Decode(&plan); err != nil {
		return releaseinstall.PurgePlan{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return releaseinstall.PurgePlan{}, errors.New("purge plan contains trailing JSON")
	}
	return plan, nil
}

func requireHostRemoved(paths ProductionPaths, currentSelection string) error {
	for label, path := range map[string]string{
		"host control endpoint":   paths.ControlEndpoint,
		"host release selection":  currentSelection,
		"host service definition": hostServiceDefinitionPath(),
	} {
		if _, err := os.Lstat(path); err == nil {
			return &lifecycleRefusalError{cause: fmt.Errorf("%s still exists at %s; run normal host removal first", label, path)}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func defaultInstallPrefix() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local")
}

func hostRemovalTargets(prefix string, paths ProductionPaths) []RemovalTarget {
	layout, _ := releaseinstall.ResolveRoleLayout(filepath.Join(prefix, "libexec", "agent-sessions"), releaseinstall.RoleHost)
	targets := make([]RemovalTarget, 0, len(hostAliasNames())+4)
	targets = append(targets, RemovalTarget{Kind: "service", Path: hostServiceDefinitionPath()})
	for _, name := range hostAliasNames() {
		targets = append(targets, RemovalTarget{Kind: "alias", Path: filepath.Join(prefix, "bin", name)})
	}
	targets = append(targets,
		RemovalTarget{Kind: "selection", Path: layout.CurrentSelection},
		RemovalTarget{Kind: "releases", Path: layout.ReleasesRoot},
		RemovalTarget{Kind: "runtime", Path: paths.ControlEndpoint},
	)
	return targets
}

func hostVendorExclusions(home string) []string {
	candidates := []struct{ environment, fallback string }{
		{"CODEX_HOME", filepath.Join(home, ".codex")},
		{"CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")},
		{"", filepath.Join(home, ".grok")},
		{"QWEN_HOME", filepath.Join(home, ".qwen")},
	}
	seen := map[string]struct{}{}
	exclusions := make([]string, 0, len(candidates)+1)
	for _, candidate := range candidates {
		path := candidate.fallback
		if candidate.environment != "" && os.Getenv(candidate.environment) != "" {
			path = filepath.Clean(os.Getenv(candidate.environment))
		}
		if cleanAbsolutePath(path) {
			if _, duplicate := seen[path]; !duplicate {
				seen[path] = struct{}{}
				exclusions = append(exclusions, path)
			}
		}
	}
	if runtimeRoot := os.Getenv("QWEN_RUNTIME_DIR"); cleanAbsolutePath(runtimeRoot) {
		if _, duplicate := seen[runtimeRoot]; !duplicate {
			exclusions = append(exclusions, runtimeRoot)
		}
	}
	return exclusions
}

func prepareHostSurfaceRemoval(prefix, runtimeEndpoint string) (func(context.Context) error, error) {
	if !cleanAbsolutePath(prefix) || !cleanAbsolutePath(runtimeEndpoint) {
		return nil, errors.New("host removal surfaces require canonical prefix and runtime endpoint")
	}
	expectedTarget := filepath.Join(prefix, "libexec", "agent-sessions", "host", "current", "bin", "agent-sessions")
	aliases, err := validateHostRemovalAliases(prefix, expectedTarget)
	if err != nil {
		return nil, err
	}
	serviceDefinition := hostServiceDefinitionPath()
	servicePresent, err := validateHostRemovalService(serviceDefinition, expectedTarget)
	if err != nil {
		return nil, err
	}
	runtimePresent, err := validateHostRemovalEndpoint(runtimeEndpoint)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		return removeHostSurfaces(ctx, aliases, serviceDefinition, servicePresent, runtimeEndpoint, runtimePresent)
	}, nil
}

func removeHostSurfaces(
	ctx context.Context,
	aliases []string,
	serviceDefinition string,
	servicePresent bool,
	runtimeEndpoint string,
	runtimePresent bool,
) error {
	var result error
	for _, alias := range aliases {
		if err := os.Remove(alias); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	if servicePresent {
		if err := os.Remove(serviceDefinition); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	if runtimePresent {
		if err := os.Remove(runtimeEndpoint); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	if runtime.GOOS == "linux" && servicePresent {
		if err := (servicecontrol.OSCommandRunner{}).Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			result = errors.Join(result, fmt.Errorf("reload removed host service definition: %w", err))
		}
	}
	return result
}

func validateHostRemovalAliases(prefix, expectedTarget string) ([]string, error) {
	aliases := make([]string, 0, len(hostAliasNames()))
	for _, name := range hostAliasNames() {
		path := filepath.Join(prefix, "bin", name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("refuse to remove non-symlink host alias %s", path)
		}
		target, err := os.Readlink(path)
		if err != nil || target != expectedTarget {
			return nil, fmt.Errorf("refuse to remove changed host alias %s", path)
		}
		aliases = append(aliases, path)
	}
	return aliases, nil
}

func validateHostRemovalService(path, expectedTarget string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxLifecyclePlanBytes {
		return false, errors.New("host service definition changed filesystem type or bound")
	}
	body, err := os.ReadFile(path) //nolint:gosec // Exact non-secret Agent Sessions service definition.
	if err != nil || !bytes.Contains(body, []byte(expectedTarget)) {
		return false, errors.New("host service definition no longer selects the installed host release")
	}
	return true, nil
}

func validateHostRemovalEndpoint(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false, errors.New("host runtime endpoint changed filesystem type")
	}
	return true, nil
}
