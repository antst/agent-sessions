package releaseinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRoleLayoutsAreDisjointAndReleaseIdentityIncludesContent(t *testing.T) {
	root := t.TempDir()
	host, err := ResolveRoleLayout(root, RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := ResolveRoleLayout(root, RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	for field, pair := range map[string][2]string{
		"release root": {host.ReleaseRoot, hub.ReleaseRoot}, "current selection": {host.CurrentSelection, hub.CurrentSelection},
		"transaction root": {host.TransactionRoot, hub.TransactionRoot}, "install lock": {host.InstallLock, hub.InstallLock},
	} {
		if pair[0] == pair[1] || strings.HasPrefix(pair[0], pair[1]+string(filepath.Separator)) ||
			strings.HasPrefix(pair[1], pair[0]+string(filepath.Separator)) {
			t.Errorf("%s is not role-disjoint: %q / %q", field, pair[0], pair[1])
		}
	}
	first, err := ReleaseID("1.2.3", testContentIdentity("a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReleaseID("1.2.3", testContentIdentity("b"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.Contains(first, "1.2.3") || !strings.Contains(second, "1.2.3") {
		t.Fatalf("same-version content identities alias: %q / %q", first, second)
	}
}

func TestInstallStagesImmutableReleaseCommitsPointerAndRequiresReadiness(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	fixture.hooks.wantReadyVersion = "1.2.3"
	fixture.hooks.wantReadyIdentity = testContentIdentity("a")
	request := fixture.release("1.2.3", "a", "agent-sessions")
	result, err := fixture.engine.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != PhaseComplete || result.ReleaseID == "" {
		t.Fatalf("install result = %+v", result)
	}
	selected, err := os.Readlink(fixture.layout.CurrentSelection)
	if err != nil {
		t.Fatal(err)
	}
	if selected != filepath.Join("releases", result.ReleaseID) {
		t.Fatalf("current selection = %q", selected)
	}
	releaseRoot := filepath.Join(fixture.layout.ReleasesRoot, result.ReleaseID)
	if fixture.hooks.preparedSource != releaseRoot || fixture.hooks.preparedSource == request.SourceRoot {
		t.Fatalf("prepare source = %q, want immutable release %q rather than input stage %q", fixture.hooks.preparedSource, releaseRoot, request.SourceRoot)
	}
	if err := filepath.Walk(releaseRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("staged release path remains writable: %s %v", path, info.Mode())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.service.calls, []string{"restart"}) ||
		!reflect.DeepEqual(fixture.hooks.calls, []string{"prepare", "ready", "commit"}) {
		t.Fatalf("service/hooks calls = %q / %q", fixture.service.calls, fixture.hooks.calls)
	}
}

func TestSameVersionDifferentContentStagesDistinctImmutableReleases(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	first, err := fixture.engine.Install(context.Background(), fixture.release("1.2.3", "a", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.engine.Install(context.Background(), fixture.release("1.2.3", "b", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ReleaseID == second.ReleaseID {
		t.Fatal("same-version releases with different bytes shared one release identity")
	}
	for _, id := range []string{first.ReleaseID, second.ReleaseID} {
		if info, statErr := os.Stat(filepath.Join(fixture.layout.ReleasesRoot, id)); statErr != nil || info.Mode().Perm()&0o222 != 0 {
			t.Errorf("release %s is missing or mutable: %v, %v", id, info, statErr)
		}
	}
}

func TestReadinessFailureRestoresExactPriorSelectionAndHookState(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	prior, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "a", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.calls, fixture.hooks.calls = nil, nil
	fixture.hooks.readyErr = errors.New("candidate identity does not match")
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "b", "agent-sessions")); err == nil {
		t.Fatal("readiness failure committed the candidate")
	}
	selected, err := os.Readlink(fixture.layout.CurrentSelection)
	if err != nil {
		t.Fatal(err)
	}
	if selected != filepath.Join("releases", prior.ReleaseID) {
		t.Fatalf("rollback selected %q, want prior %q", selected, prior.ReleaseID)
	}
	if !reflect.DeepEqual(fixture.service.calls, []string{"restart", "stop", "start", "verify"}) ||
		!reflect.DeepEqual(fixture.hooks.calls, []string{"prepare", "ready", "rollback"}) {
		t.Fatalf("rollback service/hooks calls = %q / %q", fixture.service.calls, fixture.hooks.calls)
	}
}

func TestCrashJournalRecoversPointerCommittedTransactionWithoutSecondAuthority(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	prior, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "a", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.engine.SetCrashPoint(PhasePointerCommitted)
	if _, err := fixture.engine.Install(context.Background(), fixture.release("1.1.0", "b", "agent-sessions")); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("pointer crash = %v", err)
	}
	journal, err := LoadActiveJournal(fixture.layout)
	if err != nil || journal.Phase != PhasePointerCommitted || journal.FromRelease != prior.ReleaseID {
		t.Fatalf("crash journal = %+v, %v", journal, err)
	}
	fixture.engine.SetCrashPoint("")
	if err := fixture.engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if journal, err = LoadActiveJournal(fixture.layout); err != nil || journal.Phase != PhaseComplete {
		t.Fatalf("recovered journal = %+v, %v", journal, err)
	}
	if fixture.service.maxAuthorities > 1 {
		t.Fatalf("recovery observed %d simultaneous authorities", fixture.service.maxAuthorities)
	}
}

func TestRoleNeutralRemovalPreservesStateAndPurgeIsRevisionBound(t *testing.T) {
	for _, role := range []Role{RoleHost, RoleHub} {
		t.Run(string(role), func(t *testing.T) {
			fixture := newTransactionFixture(t, role)
			if _, err := fixture.engine.Install(context.Background(), fixture.release("1.0.0", "a", roleExecutable(role))); err != nil {
				t.Fatal(err)
			}
			preserved := filepath.Join(fixture.layout.PreservedStateRoot, "identity.json")
			if err := os.MkdirAll(filepath.Dir(preserved), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(preserved, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := fixture.engine.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !containsCall(fixture.service.calls, "stop") || !containsCall(fixture.service.calls, "disable") ||
				!containsCall(fixture.hooks.calls, "remove") {
				t.Fatalf("normal removal omitted service stop/disable or role hook: %q / %q", fixture.service.calls, fixture.hooks.calls)
			}
			if _, err := os.Stat(fixture.layout.CurrentSelection); !os.IsNotExist(err) {
				t.Errorf("normal removal retained current selection: %v", err)
			}
			if body, err := os.ReadFile(preserved); err != nil || string(body) != "preserve" {
				t.Fatalf("normal removal changed preserved state: %q, %v", body, err)
			}
			plan, err := fixture.engine.PlanPurge(context.Background())
			if err != nil || plan.Role != role || plan.Revision == "" || len(plan.Targets) == 0 {
				t.Fatalf("purge plan = %+v, %v", plan, err)
			}
			if err := os.WriteFile(filepath.Join(fixture.layout.PreservedStateRoot, "revision-change"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := fixture.engine.ApplyPurge(context.Background(), plan); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("stale purge plan = %v, want revision conflict", err)
			}
		})
	}
}

func TestHostRemovalAndPurgeNeverSelectHubOwnedRoots(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
	hostLayout, err := ResolveRoleLayout(root, RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	hubLayout, err := ResolveRoleLayout(root, RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewEngine(EngineOptions{Layout: hostLayout, Service: &fakeRoleService{}, Hooks: &fakeRoleHooks{}})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewEngine(EngineOptions{Layout: hubLayout, Service: &fakeRoleService{}, Hooks: &fakeRoleHooks{}})
	if err != nil {
		t.Fatal(err)
	}
	hostSource := createReleaseSource(t, filepath.Join(root, "host-source"), "1", testContentIdentity("a"))
	hubSource := createReleaseSource(t, filepath.Join(root, "hub-source"), "9", testContentIdentity("b"))
	if _, err := host.Install(context.Background(), InstallRequest{Version: "1", ContentIdentity: testContentIdentity("a"), SourceRoot: hostSource, Executable: "agent-sessions"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Install(context.Background(), InstallRequest{Version: "9", ContentIdentity: testContentIdentity("b"), SourceRoot: hubSource, Executable: "agent-sessions-hub"}); err != nil {
		t.Fatal(err)
	}
	if err := host.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(hubLayout.CurrentSelection); err != nil {
		t.Fatalf("host removal changed hub selection: %v", err)
	}
	plan, err := host.PlanPurge(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range plan.Targets {
		if target == hubLayout.ReleaseRoot || strings.HasPrefix(target, hubLayout.ReleaseRoot+string(filepath.Separator)) {
			t.Errorf("host purge plan contains hub-owned target %q", target)
		}
	}
}

func TestPurgePlanBindsEveryTargetAndResumesAfterAnInterruptedPrefix(t *testing.T) {
	fixture := newTransactionFixture(t, RoleHost)
	first := filepath.Join(fixture.t.TempDir(), "configuration")
	second := filepath.Join(fixture.t.TempDir(), "state")
	for _, root := range []string{first, second} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "owned.json"), []byte(root), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := NewEngine(EngineOptions{
		Layout: fixture.layout, Service: fixture.service, Hooks: &fakeRoleHooks{},
		PurgeTargets: []string{first, second}, PurgeExclusions: []string{filepath.Join(fixture.t.TempDir(), "vendor")},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.PlanPurge(context.Background())
	if err != nil || len(plan.TargetRevisions) != 2 || plan.Revision == "" {
		t.Fatalf("multi-target purge plan = %+v, %v", plan, err)
	}
	if err := removePurgeTarget(first); err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplyPurge(context.Background(), plan); err != nil {
		t.Fatalf("resume purge after an already-deleted exact prefix: %v", err)
	}
	for _, root := range []string{first, second} {
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Errorf("purge retained %s: %v", root, err)
		}
	}
}

type transactionFixture struct {
	t       *testing.T
	layout  RoleLayout
	engine  *Engine
	service *fakeRoleService
	hooks   *fakeRoleHooks
	source  string
}

func newTransactionFixture(t *testing.T, role Role) transactionFixture {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { makeTestReleaseTreeWritable(root) })
	layout, err := ResolveRoleLayout(root, role)
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeRoleService{}
	hooks := &fakeRoleHooks{}
	source := filepath.Join(root, "sources")
	engine, err := NewEngine(EngineOptions{Layout: layout, Service: service, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	return transactionFixture{t: t, layout: layout, engine: engine, service: service, hooks: hooks, source: source}
}

func createReleaseSource(t *testing.T, source, version, identity string) string {
	t.Helper()
	for _, directory := range []string{"bin", "connectors", "docs"} {
		if err := os.MkdirAll(filepath.Join(source, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, executable := range []string{"agent-sessions", "agent-sessions-hub"} {
		if err := os.WriteFile(filepath.Join(source, "bin", executable), []byte("test "+executable+" image"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, payload := range []string{"codex", "claude", "grok", "qwen"} {
		if err := os.WriteFile(filepath.Join(source, "connectors", payload+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := []byte(`{"version":"` + version + `","content_identity":"` + identity + `"}`)
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return source
}

func (f transactionFixture) release(version, identitySeed, executable string) InstallRequest {
	identity := testContentIdentity(identitySeed)
	source := createReleaseSource(f.t, filepath.Join(f.source, version+"-"+identitySeed), version, identity)
	return InstallRequest{Version: version, ContentIdentity: identity, SourceRoot: source, Executable: executable}
}

type fakeRoleService struct {
	calls          []string
	authorities    int
	maxAuthorities int
}

func (s *fakeRoleService) Restart(context.Context) error {
	s.calls = append(s.calls, "restart")
	s.authorities = 1
	if s.authorities > s.maxAuthorities {
		s.maxAuthorities = s.authorities
	}
	return nil
}
func (s *fakeRoleService) Stop(context.Context) error {
	s.calls = append(s.calls, "stop")
	s.authorities = 0
	return nil
}
func (s *fakeRoleService) Disable(context.Context) error {
	s.calls = append(s.calls, "disable")
	return nil
}
func (s *fakeRoleService) Start(context.Context) error {
	s.calls = append(s.calls, "start")
	s.authorities = 1
	if s.authorities > s.maxAuthorities {
		s.maxAuthorities = s.authorities
	}
	return nil
}
func (s *fakeRoleService) Verify(context.Context) error {
	s.calls = append(s.calls, "verify")
	return nil
}

type fakeRoleHooks struct {
	calls             []string
	readyErr          error
	wantReadyVersion  string
	wantReadyIdentity string
	preparedSource    string
}

func (h *fakeRoleHooks) Prepare(_ context.Context, request InstallRequest) error {
	h.calls = append(h.calls, "prepare")
	h.preparedSource = request.SourceRoot
	return nil
}
func (h *fakeRoleHooks) Ready(_ context.Context, release InstalledRelease) error {
	h.calls = append(h.calls, "ready")
	if h.wantReadyVersion != "" && (release.Version != h.wantReadyVersion || release.ContentIdentity != h.wantReadyIdentity) {
		return errors.New("readiness received the wrong release identity")
	}
	return h.readyErr
}
func (h *fakeRoleHooks) Commit(context.Context) error {
	h.calls = append(h.calls, "commit")
	return nil
}
func (h *fakeRoleHooks) Rollback(context.Context) error {
	h.calls = append(h.calls, "rollback")
	return nil
}
func (h *fakeRoleHooks) Remove(context.Context) error {
	h.calls = append(h.calls, "remove")
	return nil
}

func roleExecutable(role Role) string {
	if role == RoleHub {
		return "agent-sessions-hub"
	}
	return "agent-sessions"
}

func containsCall(calls []string, wanted string) bool {
	for _, call := range calls {
		if call == wanted {
			return true
		}
	}
	return false
}

func testContentIdentity(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)
}

func makeTestReleaseTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
