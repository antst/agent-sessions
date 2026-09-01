package daemon

import (
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestLifecycleTransitionsAreMonotonicAndProductNeutral(t *testing.T) {
	for _, test := range []struct {
		kind     string
		from, to string
		want     bool
	}{
		{kind: "attachment", from: "preparing", to: "prepared", want: true},
		{kind: "attachment", from: "prepared", to: "selecting", want: true},
		{kind: "attachment", from: "selecting", to: "attached", want: true},
		{kind: "attachment", from: "attached", to: "detaching", want: true},
		{kind: "attachment", from: "detaching", to: "detached", want: true},
		{kind: "attachment", from: "attached", to: "debt", want: true},
		{kind: "attachment", from: "detached", to: "attached"},
		{kind: "delivery", from: "prepared", to: "accepted", want: true},
		{kind: "delivery", from: "accepted", to: "presented", want: true},
		{kind: "delivery", from: "presented", to: "acknowledged", want: true},
		{kind: "delivery", from: "accepted", to: "retryable", want: true},
		{kind: "delivery", from: "acknowledged", to: "accepted"},
		{kind: "lane", from: "preparing", to: "idle", want: true},
		{kind: "lane", from: "preparing", to: "running", want: true},
		{kind: "lane", from: "idle", to: "preparing", want: true},
		{kind: "lane", from: "idle", to: "running", want: true},
		{kind: "lane", from: "running", to: "interrupting", want: true},
		{kind: "lane", from: "interrupting", to: "terminal", want: true},
		{kind: "lane", from: "terminal", to: "archived", want: true},
		{kind: "lane", from: "archived", to: "idle", want: true},
		{kind: "lane", from: "archived", to: "running"},
		{kind: "turn", from: "accepted", to: "dispatched", want: true},
		{kind: "turn", from: "dispatched", to: "terminal", want: true},
		{kind: "turn", from: "terminal", to: "collected", want: true},
		{kind: "turn", from: "collected", to: "dispatched"},
	} {
		t.Run(test.kind+"/"+test.from+"/"+test.to, func(t *testing.T) {
			if got := ValidLifecycleTransition(test.kind, test.from, test.to); got != test.want {
				t.Fatalf("ValidLifecycleTransition(%q,%q,%q) = %v, want %v", test.kind, test.from, test.to, got, test.want)
			}
		})
	}
}

func TestStateStoreRoundTripsEveryDurableEntityAndRetainsCleanupDebt(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{
		Host: HostRuntime{User: "1000", Host: "pdev", Release: "candidate", Generation: 7, Endpoint: "/run/agent-sessions.sock", ServiceState: "running"},
		Attachments: map[string]ManagedAttachment{
			"attachment": {ID: "attachment", Product: "codex", NativeSessionID: "thread", Name: "worker", Cwd: "/workspace", Groups: []string{"project"}, PermissionMode: "default", State: "attached", Evidence: NativeEvidence{Process: procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}, Executable: "/bin/codex", ThreadID: "thread"}},
		},
		Deliveries: map[string]Delivery{
			"message": {ID: "message", Sender: "source", Destinations: []string{"target"}, Groups: []string{"project"}, State: "accepted"},
		},
		Lanes: map[string]Lane{
			"lane": {ID: "lane", ParentAttachmentID: "attachment", Product: "grok", NativeSessionID: "native", Cwd: "/workspace", Groups: []string{"project"}, PermissionMode: "bypass", State: "running"},
		},
		Turns: map[string]Turn{
			"turn": {ID: "turn", LaneID: "lane", Sequence: 1, State: "dispatched"},
		},
		CleanupDebts: map[string]CleanupDebt{
			"debt": {ID: "debt", Resource: "/owned/socket", BaselineIdentity: "socket-revision", IntendedState: "absent", LastVerifiedState: "unknown", Cause: "identity unavailable", RetryRevision: 3, Operation: "detach-codex"},
		},
	}
	committed, err := store.Commit(0, catalog)
	if err != nil || committed.Revision != 1 {
		t.Fatalf("commit = %+v, %v", committed, err)
	}
	loaded, err := store.Read()
	if err != nil || !reflect.DeepEqual(loaded.Catalog, catalog) {
		t.Fatalf("round trip = %+v, %v; want %+v", loaded, err, catalog)
	}
	updated := loaded.Catalog
	updated.Host.Generation++
	second, err := store.Commit(loaded.Revision, updated)
	if err != nil || second.Catalog.CleanupDebts["debt"].RetryRevision != 3 {
		t.Fatalf("cleanup debt was lost across unrelated update: %+v, %v", second, err)
	}
}

func TestStateStoreRejectsInvalidLifecycleAndStaleRevisionWithoutMutation(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	baseline := Catalog{Host: HostRuntime{User: "1000", Host: "pdev", Generation: 1}, Attachments: map[string]ManagedAttachment{
		"attachment": {ID: "attachment", Product: "claude", State: "attached"},
	}}
	first, err := store.Commit(0, baseline)
	if err != nil {
		t.Fatal(err)
	}
	invalid := first.Catalog
	attachment := invalid.Attachments["attachment"]
	attachment.State = "preparing"
	invalid.Attachments["attachment"] = attachment
	if _, err := store.Commit(first.Revision, invalid); err == nil {
		t.Fatal("backward attachment transition was accepted")
	}
	if _, err := store.Commit(0, baseline); err == nil {
		t.Fatal("stale catalog revision was accepted")
	}
	loaded, err := store.Read()
	if err != nil || loaded.Revision != first.Revision || loaded.Catalog.Attachments["attachment"].State != "attached" {
		t.Fatalf("state changed after rejected commits: %+v, %v", loaded, err)
	}
}
