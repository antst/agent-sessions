package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRemovalRefusesEveryActiveBlockerBeforeMutation(t *testing.T) {
	root := t.TempDir()
	plan := RemovalPlan{
		Role: "host", Revision: "remove-r1",
		Blockers: []RemovalBlocker{
			{Kind: "attachment", ID: "peer-1", State: "ready"},
			{Kind: "lane", ID: "lane-1", State: "running"},
		},
		Targets: []RemovalTarget{{Kind: "service", Path: filepath.Join(root, "agent-sessions.service")}},
	}
	mutations := 0
	_, err := ApplyRemoval(context.Background(), plan, RemovalHooks{
		StopService:  func(context.Context) error { mutations++; return nil },
		RemoveTarget: func(context.Context, RemovalTarget) error { mutations++; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "peer-1") || !strings.Contains(err.Error(), "lane-1") {
		t.Fatalf("blocker refusal = %v", err)
	}
	if mutations != 0 {
		t.Fatalf("blocked removal performed %d mutations", mutations)
	}
}

func TestNormalRemovalPreservesAgentSessionsStateAndVendorData(t *testing.T) {
	root := t.TempDir()
	configuration := filepath.Join(root, "config", "config.json")
	durableState := filepath.Join(root, "state")
	vendorProfile := filepath.Join(root, "vendor", "codex")
	targets := []RemovalTarget{
		{Kind: "service", Path: filepath.Join(root, "service", "agent-sessions.service")},
		{Kind: "alias", Path: filepath.Join(root, "bin", "codex-peer")},
		{Kind: "release", Path: filepath.Join(root, "releases", "0.3.0")},
		{Kind: "runtime", Path: filepath.Join(root, "run", "daemon.sock")},
	}
	plan := RemovalPlan{
		Role: "host", Revision: "remove-r2", Targets: targets,
		Preserved: []string{configuration, durableState}, Exclusions: []string{vendorProfile},
	}
	var removed []string
	connectorsRemoved := 0
	result, err := ApplyRemoval(context.Background(), plan, RemovalHooks{
		StopService:      func(context.Context) error { return nil },
		RemoveConnectors: func(context.Context) error { connectorsRemoved++; return nil },
		RemoveTarget: func(_ context.Context, target RemovalTarget) error {
			removed = append(removed, target.Path)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if connectorsRemoved != 1 || !reflect.DeepEqual(removed, []string{targets[0].Path, targets[1].Path, targets[2].Path, targets[3].Path}) {
		t.Fatalf("removal result/hooks = %+v, connectors=%d removed=%v", result, connectorsRemoved, removed)
	}
	for _, forbidden := range []string{configuration, durableState, vendorProfile} {
		if slicesContain(result.Deleted, forbidden) {
			t.Fatalf("normal removal deleted preserved/excluded path %q", forbidden)
		}
	}
}

func TestOfflinePurgeRequiresExactRevisionAndRejectsVendorTargets(t *testing.T) {
	root := t.TempDir()
	vendor := filepath.Join(root, "vendor")
	valid := PurgePlan{
		Role: "host", PlanRevision: "purge-r1", StateRevision: "state-r7",
		Targets:    []RemovalTarget{{Kind: "configuration", Path: filepath.Join(root, "agent-sessions", "config.json")}},
		Exclusions: []string{vendor},
	}
	mutations := 0
	if _, err := ApplyPurge(context.Background(), valid, "state-r8", func(context.Context, RemovalTarget) error {
		mutations++
		return nil
	}); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("stale purge error = %v", err)
	}
	if mutations != 0 {
		t.Fatalf("stale purge performed %d mutations", mutations)
	}

	invalid := valid
	invalid.Targets = []RemovalTarget{{Kind: "credential", Path: filepath.Join(vendor, "auth.json")}}
	if _, err := ApplyPurge(context.Background(), invalid, valid.StateRevision, func(context.Context, RemovalTarget) error {
		mutations++
		return nil
	}); err == nil || !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("vendor exclusion error = %v", err)
	}
	if mutations != 0 {
		t.Fatalf("excluded purge performed %d mutations", mutations)
	}
}

func TestInterruptedPurgeReturnsExactRetryableDebtAndResumes(t *testing.T) {
	root := t.TempDir()
	first := RemovalTarget{Kind: "configuration", Path: filepath.Join(root, "config.json")}
	second := RemovalTarget{Kind: "state", Path: filepath.Join(root, "state")}
	plan := PurgePlan{Role: "host", PlanRevision: "purge-r2", StateRevision: "state-r9", Targets: []RemovalTarget{first, second}}
	injected := errors.New("injected interruption")
	result, err := ApplyPurge(context.Background(), plan, plan.StateRevision, func(_ context.Context, target RemovalTarget) error {
		if target.Path == second.Path {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) || !reflect.DeepEqual(result.Deleted, []string{first.Path}) || len(result.Debt) != 1 || result.Debt[0].ResourceIdentity != second.Path {
		t.Fatalf("interrupted purge result = %+v, err=%v", result, err)
	}

	result, err = ApplyPurge(context.Background(), plan, plan.StateRevision, func(context.Context, RemovalTarget) error { return nil })
	if err != nil || !reflect.DeepEqual(result.Deleted, []string{first.Path, second.Path}) || len(result.Debt) != 0 {
		t.Fatalf("retried purge result = %+v, err=%v", result, err)
	}
}

func TestHostPurgeCLIUsesCanonicalAgentSessionsRootsAndExcludesVendorProfiles(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	runtimeBase, err := os.MkdirTemp("", "as-prg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeBase) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	paths, err := ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	paths.RuntimeRoot = filepath.Join(runtimeBase, stateDirectoryName)
	paths.ControlEndpoint = filepath.Join(paths.RuntimeRoot, controlSocketName)
	for _, owned := range []string{paths.ConfigurationRoot, paths.StateRoot} {
		if err := os.MkdirAll(owned, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(owned, "owned.json"), []byte("owned"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	vendor := filepath.Join(home, ".codex")
	if err := os.MkdirAll(vendor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendor, "auth.json"), []byte("opaque"), 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "purge-plan.json")
	prefix := defaultInstallPrefix()
	inspection, err := runHostPurgeInspectCLI(context.Background(), planPath, prefix, paths)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Role != "host" || !reflect.DeepEqual(inspection.Targets, []string{paths.ConfigurationRoot, paths.StateRoot}) ||
		!slicesContain(inspection.Exclusions, vendor) {
		t.Fatalf("host purge inspection = %+v", inspection)
	}
	result, err := runHostPurgeApplyCLI(context.Background(), planPath, prefix, paths)
	if err != nil || !reflect.DeepEqual(result.Deleted, inspection.Targets) {
		t.Fatalf("host purge apply = %+v, %v", result, err)
	}
	for _, owned := range inspection.Targets {
		if _, err := os.Lstat(owned); !os.IsNotExist(err) {
			t.Errorf("purge retained Agent Sessions root %s: %v", owned, err)
		}
	}
	if _, err := os.Stat(filepath.Join(vendor, "auth.json")); err != nil {
		t.Fatalf("host purge touched vendor profile: %v", err)
	}
}

func TestHostSurfaceRemovalFailsClosedBeforeChangedAliasMutation(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "prefix")
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(bin, "agent-sessions")
	wantTarget := filepath.Join(prefix, "libexec", "agent-sessions", "host", "current", "bin", "agent-sessions")
	if err := os.Symlink(wantTarget, good); err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(bin, "codex-peer")
	if err := os.Symlink(filepath.Join(root, "unrelated"), changed); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareHostSurfaceRemoval(prefix, filepath.Join(root, "daemon.sock")); err == nil || !strings.Contains(err.Error(), "changed host alias") {
		t.Fatalf("changed host alias preflight = %v", err)
	}
	if target, err := os.Readlink(good); err != nil || target != wantTarget {
		t.Fatalf("failed preflight mutated an earlier exact alias: %q, %v", target, err)
	}
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
