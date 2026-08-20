package federator

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSessionCatalogRestoresAndReplacesGroupsAndYolo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	catalog, err := openSessionCatalog(path, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	catalog.now = func() int64 { return 10 }
	preference, groups, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "session-a", Product: "codex", ExplicitGroups: []string{"review", "project-a"},
		GroupsSpecified: true, AlwaysApprove: true, AlwaysApproveSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !preference.AlwaysApprove || preference.Product != "codex" || preference.UpdatedAt != 10 {
		t.Fatalf("preference = %+v", preference)
	}
	wantGroups := []string{"project-a", "review", "session:host-a/session-a"}
	if !reflect.DeepEqual(groups, wantGroups) {
		t.Fatalf("effective groups = %v; want %v", groups, wantGroups)
	}

	reloaded, err := openSessionCatalog(path, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = func() int64 { return 20 }
	preference, groups, err = reloaded.update(SessionPreferenceUpdate{SessionID: "session-a", Product: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if !preference.AlwaysApprove || !reflect.DeepEqual(groups, wantGroups) {
		t.Fatalf("omitted resume did not restore preferences: %+v groups=%v", preference, groups)
	}

	preference, groups, err = reloaded.update(SessionPreferenceUpdate{
		SessionID: "session-a", Product: "codex", ExplicitGroups: []string{"project-b"}, GroupsSpecified: true,
		AlwaysApprove: false, AlwaysApproveSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preference.AlwaysApprove || !reflect.DeepEqual(groups, []string{"project-b", "session:host-a/session-a"}) {
		t.Fatalf("explicit resume did not replace preferences: %+v groups=%v", preference, groups)
	}
}

func TestSessionCatalogPreviewDoesNotAdoptOrModifySession(t *testing.T) {
	root := t.TempDir()
	catalog, err := openSessionCatalog(filepath.Join(root, "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-4000-8000-000000000204"
	preview, groups, err := catalog.preview(SessionPreferenceUpdate{
		SessionID: sessionID, Product: "claude", Kind: SessionKindInteractive,
		ExplicitGroups: []string{"preview-only"}, GroupsSpecified: true,
		AlwaysApprove: true, AlwaysApproveSpecified: true,
	})
	if err != nil || !preview.AlwaysApprove || !slices.Contains(groups, "preview-only") {
		t.Fatalf("catalog preview = %+v groups=%v err=%v", preview, groups, err)
	}
	if _, _, exists, err := catalog.get(sessionID); err != nil || exists {
		t.Fatalf("catalog preview persisted: exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("catalog preview wrote durable state: %v", err)
	}
}

func TestSessionCatalogPreparedAdoptionRollsBackToAbsence(t *testing.T) {
	catalog, err := openSessionCatalog(filepath.Join(t.TempDir(), "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	update := SessionPreferenceUpdate{
		SessionID: "00000000-0000-4000-8000-000000000205", Product: "claude", Kind: SessionKindInteractive,
		ExplicitGroups: []string{"temporary"}, GroupsSpecified: true,
	}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	var journalPrior *SessionPreferences
	desired, _, err := catalog.prepare(update, expected, func(prior *SessionPreferences, _ SessionPreferences) error {
		journalPrior = prior
		return nil
	}, func() error { return nil })
	if err != nil || journalPrior != nil {
		t.Fatalf("prepared adoption = %+v prior=%+v err=%v", desired, journalPrior, err)
	}
	if restored, err := catalog.restorePrepared(desired, journalPrior); err != nil || !restored {
		t.Fatalf("rollback prepared adoption: restored=%v err=%v", restored, err)
	}
	if _, _, exists, err := catalog.get(update.SessionID); err != nil || exists {
		t.Fatalf("rolled-back adoption remains: exists=%v err=%v", exists, err)
	}
}

func TestSessionCatalogRollbackDoesNotOverwriteLaterSameValueRevision(t *testing.T) {
	catalog, err := openSessionCatalog(filepath.Join(t.TempDir(), "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	catalog.now = func() int64 { return 50 }
	update := SessionPreferenceUpdate{
		SessionID: "00000000-0000-4000-8000-000000000206", Product: "claude", Kind: SessionKindInteractive,
		ExplicitGroups: []string{"same-value"}, GroupsSpecified: true,
	}
	expected, _, err := catalog.preview(update)
	if err != nil {
		t.Fatal(err)
	}
	desired, _, err := catalog.prepare(update, expected, func(*SessionPreferences, SessionPreferences) error {
		return nil
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	later, _, err := catalog.update(update)
	if err != nil || later.UpdatedAt <= desired.UpdatedAt || later.Revision == desired.Revision {
		t.Fatalf("later same-value update revision = %d/%s, desired = %d/%s, err=%v", later.UpdatedAt, later.Revision, desired.UpdatedAt, desired.Revision, err)
	}
	if restored, err := catalog.restorePrepared(desired, nil); err != nil || restored {
		t.Fatalf("rollback overwrote later same-value revision: restored=%v err=%v", restored, err)
	}
	current, _, exists, err := catalog.get(update.SessionID)
	if err != nil || !exists || current.UpdatedAt != later.UpdatedAt {
		t.Fatalf("later revision changed: current=%+v exists=%v err=%v", current, exists, err)
	}
}

func TestSessionCatalogChildAlwaysGetsParentAnchorAndOptionallyInheritsGroups(t *testing.T) {
	catalog, err := openSessionCatalog(filepath.Join(t.TempDir(), "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = catalog.update(SessionPreferenceUpdate{
		SessionID: "parent", Product: "grok", ExplicitGroups: []string{"project-a", "review"}, GroupsSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	preference, groups, err := catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "codex", ExplicitGroups: []string{"special"}, GroupsSpecified: true,
		ParentSession: "parent", ParentSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preference.ParentSession != "parent" {
		t.Fatalf("parent = %q", preference.ParentSession)
	}
	want := []string{"session:host-a/child", "session:host-a/parent", "special"}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("effective groups = %v; want %v", groups, want)
	}
	preference, groups, err = catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "codex", InheritParentGroups: true, InheritGroupsSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !preference.InheritParentGroups {
		t.Fatal("explicit parent-group inheritance was not persisted")
	}
	want = []string{"project-a", "review", "session:host-a/child", "session:host-a/parent", "special"}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("opt-in effective groups = %v; want %v", groups, want)
	}

	_, _, err = catalog.update(SessionPreferenceUpdate{
		SessionID: "parent", Product: "grok", ExplicitGroups: []string{"project-b"}, GroupsSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, groups, ok, err := catalog.get("child")
	if err != nil || !ok {
		t.Fatalf("get child: ok=%v err=%v", ok, err)
	}
	want = []string{"project-a", "review", "session:host-a/child", "session:host-a/parent", "special"}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("live child inheritance changed = %v; want %v", groups, want)
	}
	_, groups, err = catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "codex", ParentSession: "parent", ParentSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"project-a", "review", "session:host-a/child", "session:host-a/parent", "special"}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("plain resume widened inherited groups = %v; want %v", groups, want)
	}
	_, groups, err = catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "codex", InheritParentGroups: true, InheritGroupsSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"project-b", "session:host-a/child", "session:host-a/parent", "special"}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("explicit inheritance refresh = %v; want %v", groups, want)
	}
	_, groups, err = catalog.update(SessionPreferenceUpdate{
		SessionID: "child", Product: "codex", InheritParentGroups: false, InheritGroupsSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"session:host-a/child", "session:host-a/parent", "special"}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("explicit no-inherit groups = %v; want %v", groups, want)
	}
}

func TestSessionCatalogRejectsReservedDuplicateSelfParentAndProductChange(t *testing.T) {
	catalog, err := openSessionCatalog(filepath.Join(t.TempDir(), "sessions.json"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	bad := []SessionPreferenceUpdate{
		{SessionID: "a", Product: "codex", ExplicitGroups: []string{"session:forged"}, GroupsSpecified: true},
		{SessionID: "a", Product: "codex", ExplicitGroups: []string{"same", "same"}, GroupsSpecified: true},
	}
	for _, update := range bad {
		if _, _, err := catalog.update(update); err == nil {
			t.Fatalf("update unexpectedly accepted: %+v", update)
		}
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: "a", Product: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: "a", Product: "grok"}); err == nil {
		t.Fatal("product change unexpectedly accepted")
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: "b", Product: "grok", ParentSession: "a", ParentSpecified: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: "a", Product: "codex", ParentSession: "a", ParentSpecified: true}); err == nil {
		t.Fatal("self parent unexpectedly accepted")
	}
}

func TestSessionCatalogMaximumPrivateAnchorReopens(t *testing.T) {
	hostID := strings.Repeat("h", 48)
	parentID := strings.Repeat("p", maxSessionIDBytes)
	childID := strings.Repeat("c", maxSessionIDBytes)
	path := filepath.Join(t.TempDir(), "sessions.json")
	catalog, err := openSessionCatalog(path, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.update(SessionPreferenceUpdate{SessionID: parentID, Product: "claude"}); err != nil {
		t.Fatal(err)
	}
	_, want, err := catalog.update(SessionPreferenceUpdate{
		SessionID: childID, Product: "grok", ParentSession: parentID, ParentSpecified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := openSessionCatalog(path, hostID)
	if err != nil {
		t.Fatalf("reopen maximum private anchor: %v", err)
	}
	_, got, ok, err := reopened.get(childID)
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened groups = %v, ok=%v, err=%v; want %v", got, ok, err, want)
	}
}
