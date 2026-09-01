package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func validNewDomainCatalog(t *testing.T) (Catalog, NativeSessionLeaseKey) {
	t.Helper()
	digest := sha256.Sum256([]byte("input"))
	leaseKey, err := NewNativeSessionLeaseKey("codex", "profile", "lane-native")
	if err != nil {
		t.Fatal(err)
	}
	return Catalog{
		Host: HostRuntime{User: "1000", Host: "pdev", Generation: 7},
		Attachments: map[string]ManagedAttachment{
			// Attachment IDs and native session IDs intentionally occupy distinct
			// namespaces. Explicit resume may bind a fresh attachment to an old
			// native transcript.
			"attachment-fresh": {ID: "attachment-fresh", Product: "codex", NativeSessionID: "native-transcript-existing", State: "attached"},
		},
		Deliveries: map[string]Delivery{},
		Lanes: map[string]Lane{
			"lane": {ID: "lane", ParentAttachmentID: "attachment-fresh", Product: "codex", ProfileIdentity: "profile", NativeSessionID: "lane-native", InputSequence: 1, State: "running"},
		},
		Turns: map[string]Turn{
			"turn": {ID: "turn", LaneID: "lane", Sequence: 1, State: "dispatched"},
		},
		CleanupDebts: map[string]CleanupDebt{},
		LaneInputs: map[string]LaneInputReceipt{
			"receipt": {Schema: LaneInputReceiptRecordSchema, ReceiptID: "receipt", LaneID: "lane", Sequence: 1, Digest: digest, Bytes: 5, SpoolObjectID: "spool", State: ReceiptQueued, Revision: 1, AcceptedAt: 10, UpdatedAt: 10},
		},
		NativeLeases: map[NativeSessionLeaseKey]NativeSessionLease{
			leaseKey: {Schema: NativeSessionLeaseRecordSchema, ProductID: "codex", ProfileIdentity: "profile", NativeSessionID: "lane-native", OwnerLaneID: "lane", Generation: 7, State: LeasePrepared, Revision: 1, CreatedAt: 10, UpdatedAt: 10},
		},
		ComponentBindings: map[string]ComponentBinding{
			"binding": {Schema: ComponentBindingRecordSchema, BindingID: "binding", AttachmentID: "attachment-fresh", ProcessIdentity: procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}, BootstrapRevision: 1, Generation: 7, State: BindingBinding},
		},
		ComponentSessions: map[string]ComponentSession{
			"attachment-fresh": {Schema: ComponentSessionRecordSchema, AttachmentID: "attachment-fresh", BindingID: "binding", NativeSessionID: "native-transcript-existing", State: ComponentSessionAnnounced, LastEventSeq: 1},
		},
	}, leaseKey
}

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
			"attachment": {ID: "attachment", Product: "codex", NativeSessionID: "thread", Name: "worker", Cwd: "/workspace", Groups: []string{"project"}, PermissionMode: "default", State: "attached", Evidence: NativeEvidence{Process: procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}, Ancestry: []procinfo.Identity{{PID: 41, Start: "parent", StrongStart: "parent-strong"}}, Executable: "/bin/codex", ThreadID: "thread"}},
		},
		Deliveries: map[string]Delivery{
			"message": {ID: "message", Sender: "source", Destinations: []string{"target"}, Groups: []string{"project"}, State: "accepted"},
		},
		Lanes: map[string]Lane{
			"lane": {ID: "lane", ParentAttachmentID: "attachment", Product: "grok", ProfileIdentity: "profile", NativeSessionID: "native", InputSequence: 1, Cwd: "/workspace", Groups: []string{"project"}, PermissionMode: "bypass", State: "running"},
		},
		Turns: map[string]Turn{
			"turn": {ID: "turn", LaneID: "lane", Sequence: 1, State: "dispatched"},
		},
		CleanupDebts: map[string]CleanupDebt{
			"debt": {ID: "debt", Resource: "/owned/socket", BaselineIdentity: "socket-revision", IntendedState: "absent", LastVerifiedState: "unknown", Cause: "identity unavailable", RetryRevision: 3, Operation: "detach-codex"},
		},
	}
	digest := sha256.Sum256([]byte("input"))
	catalog.LaneInputs = map[string]LaneInputReceipt{
		"receipt": {Schema: LaneInputReceiptRecordSchema, ReceiptID: "receipt", LaneID: "lane", Sequence: 1, Digest: digest, Bytes: 5, SpoolObjectID: "spool", State: ReceiptQueued, Revision: 1, AcceptedAt: 10, UpdatedAt: 10},
	}
	leaseKey, err := NewNativeSessionLeaseKey("grok", "profile", "native")
	if err != nil {
		t.Fatal(err)
	}
	catalog.NativeLeases = map[NativeSessionLeaseKey]NativeSessionLease{
		leaseKey: {Schema: NativeSessionLeaseRecordSchema, ProductID: "grok", ProfileIdentity: "profile", NativeSessionID: "native", OwnerLaneID: "lane", Generation: 7, State: LeasePrepared, Revision: 1, CreatedAt: 10, UpdatedAt: 10},
	}
	catalog.ComponentBindings = map[string]ComponentBinding{
		"binding": {Schema: ComponentBindingRecordSchema, BindingID: "binding", AttachmentID: "attachment", ProcessIdentity: procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}, BootstrapRevision: 1, Generation: 7, State: BindingBinding},
	}
	catalog.ComponentSessions = map[string]ComponentSession{
		"attachment": {Schema: ComponentSessionRecordSchema, AttachmentID: "attachment", BindingID: "binding", NativeSessionID: "thread", State: ComponentSessionAnnounced, LastEventSeq: 1},
	}
	committed, err := store.Commit(0, catalog)
	if err != nil || committed.Revision != 1 {
		t.Fatalf("commit = %+v, %v", committed, err)
	}
	first := committed.Catalog
	receipt := first.LaneInputs["receipt"]
	receipt.State, receipt.TargetTurnID, receipt.DispatchAttempt, receipt.Revision, receipt.UpdatedAt = ReceiptDispatching, "turn", "attempt", 2, 11
	first.LaneInputs["receipt"] = receipt
	lease := first.NativeLeases[leaseKey]
	lease.State, lease.ProcessGroup, lease.Revision, lease.UpdatedAt = LeaseHeld, procinfo.Identity{PID: 45, Start: "lane-start", StrongStart: "lane-strong"}, 2, 11
	first.NativeLeases[leaseKey] = lease
	binding := first.ComponentBindings["binding"]
	binding.State, binding.LastInboundSeq, binding.LastOutboundSeq = BindingReady, 3, 4
	first.ComponentBindings["binding"] = binding
	session := first.ComponentSessions["attachment"]
	session.State, session.LastEventSeq = ComponentSessionIdle, 5
	first.ComponentSessions["attachment"] = session
	committed, err = store.Commit(committed.Revision, first)
	if err != nil {
		t.Fatal(err)
	}
	secondCatalog := committed.Catalog
	receipt = secondCatalog.LaneInputs["receipt"]
	receipt.State, receipt.NativeAcceptance, receipt.Revision, receipt.UpdatedAt = ReceiptInjected, &NativeAcceptanceRef{NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 12}, 3, 12
	secondCatalog.LaneInputs["receipt"] = receipt
	committed, err = store.Commit(committed.Revision, secondCatalog)
	if err != nil {
		t.Fatal(err)
	}
	callerCopy := committed.Catalog
	callerAttachment := callerCopy.Attachments["attachment"]
	callerAttachment.Groups[0] = "mutated-group"
	callerAttachment.Evidence.Ancestry[0].Start = "mutated-parent"
	callerCopy.Attachments["attachment"] = callerAttachment
	callerCopy.LaneInputs["receipt"].NativeAcceptance.NativeMessageID = "mutated-message"
	loaded, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Catalog.Attachments["attachment"].Groups[0] != "project" ||
		loaded.Catalog.Attachments["attachment"].Evidence.Ancestry[0].Start != "parent" ||
		loaded.Catalog.LaneInputs["receipt"].NativeAcceptance.NativeMessageID != "message" {
		t.Fatalf("nested caller mutation leaked into state store: %+v", loaded.Catalog)
	}
	updated := loaded.Catalog
	updated.Host.Release = "candidate-two"
	fourth, err := store.Commit(loaded.Revision, updated)
	if err != nil || fourth.Catalog.CleanupDebts["debt"].RetryRevision != 3 {
		t.Fatalf("cleanup debt was lost across unrelated update: %+v, %v", fourth, err)
	}
}

func TestStateStoreNormalizesOldCatalogMapsAndReturnsDeepIsolation(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	legacyBody := json.RawMessage(`{"host":{"user":"1000","host":"pdev","generation":1},"attachments":{},"deliveries":{},"lanes":{},"turns":{},"cleanup_debts":{}}`)
	if _, err := store.store.Commit(0, map[string]json.RawMessage{catalogRecord: legacyBody}); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if committed.Catalog.LaneInputs == nil || committed.Catalog.NativeLeases == nil || committed.Catalog.ComponentBindings == nil || committed.Catalog.ComponentSessions == nil {
		t.Fatalf("new maps were not normalized: %#v", committed.Catalog)
	}
	committed.Catalog.Attachments["mutated"] = ManagedAttachment{ID: "mutated"}
	loaded, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := loaded.Catalog.Attachments["mutated"]; leaked {
		t.Fatal("caller mutation leaked into state store")
	}
}

func TestNewDurableEntityValidationAndTransitionsFailClosed(t *testing.T) {
	if !ValidLifecycleTransition("receipt", "dispatching", "ambiguous") || ValidLifecycleTransition("receipt", "ambiguous", "queued") {
		t.Fatal("receipt transition contract is wrong")
	}
	if !ValidLifecycleTransition("receipt", "ambiguous", "injected") {
		t.Fatal("proven native acceptance cannot resolve an ambiguous receipt")
	}
	if !ValidLifecycleTransition("lease", "held", "releasing") || ValidLifecycleTransition("lease", "held", "released") {
		t.Fatal("lease transition contract is wrong")
	}
	if !ValidLifecycleTransition("component-binding", "ready", "retiring") || ValidLifecycleTransition("component-binding", "closed", "ready") {
		t.Fatal("component binding transition contract is wrong")
	}
	if !ValidLifecycleTransition("component-session", "busy", "idle") || ValidLifecycleTransition("component-session", "closed", "busy") {
		t.Fatal("component session transition contract is wrong")
	}

	keyA, err := NewNativeSessionLeaseKey("dsh", "profile/one", "native:one")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := NewNativeSessionLeaseKey("dsh", "profile", "one/native:one")
	if err != nil {
		t.Fatal(err)
	}
	if keyA == keyB {
		t.Fatalf("composite lease keys collided: %q", keyA)
	}
	if _, err := NewNativeSessionLeaseKey("dsh", "", "native"); err == nil {
		t.Fatal("empty lease key field accepted")
	}

	base, _ := validNewDomainCatalog(t)
	if err := validateCatalog(base); err != nil {
		t.Fatalf("valid new durable domains rejected: %v", err)
	}
	if base.ComponentSessions["attachment-fresh"].AttachmentID == base.ComponentSessions["attachment-fresh"].NativeSessionID {
		t.Fatal("fixture failed to prove attachment/native session namespace separation")
	}
}

func TestDurableRecordSchemasRejectMissingAndUnknownVersions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Catalog, NativeSessionLeaseKey, RecordSchema)
	}{
		{name: "receipt", mutate: func(c *Catalog, _ NativeSessionLeaseKey, schema RecordSchema) {
			row := c.LaneInputs["receipt"]
			row.Schema = schema
			c.LaneInputs["receipt"] = row
		}},
		{name: "lease", mutate: func(c *Catalog, key NativeSessionLeaseKey, schema RecordSchema) {
			row := c.NativeLeases[key]
			row.Schema = schema
			c.NativeLeases[key] = row
		}},
		{name: "binding", mutate: func(c *Catalog, _ NativeSessionLeaseKey, schema RecordSchema) {
			row := c.ComponentBindings["binding"]
			row.Schema = schema
			c.ComponentBindings["binding"] = row
		}},
		{name: "session", mutate: func(c *Catalog, _ NativeSessionLeaseKey, schema RecordSchema) {
			row := c.ComponentSessions["attachment-fresh"]
			row.Schema = schema
			c.ComponentSessions["attachment-fresh"] = row
		}},
	}
	for _, test := range tests {
		for _, schema := range []RecordSchema{"", "agent-sessions." + RecordSchema(test.name) + ".v999"} {
			t.Run(test.name+"/"+string(schema), func(t *testing.T) {
				catalog, key := validNewDomainCatalog(t)
				test.mutate(&catalog, key, schema)
				if err := validateCatalog(catalog); err == nil {
					t.Fatalf("schema %q was accepted", schema)
				}
			})
		}
	}

	receipt := LaneInputReceipt{
		Schema: LaneInputReceiptRecordSchema,
		NativeAcceptance: &NativeAcceptanceRef{
			NativeSessionID: "native",
			NativeMessageID: "message",
			AcceptedAt:      1,
		},
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil {
		t.Fatal(err)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(outer["native_acceptance"], &nested); err != nil {
		t.Fatal(err)
	}
	if _, hasIndependentSchema := nested["schema"]; hasIndependentSchema {
		t.Fatal("NativeAcceptanceRef acquired an independent record schema")
	}
}

func TestLaneInputReceiptStateSpecificValidationFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Catalog)
	}{
		{name: "oversize", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.Bytes = maxLaneInputReceiptBytes + 1
			c.LaneInputs["receipt"] = row
		}},
		{name: "path-like-spool", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.SpoolObjectID = "../secret"
			c.LaneInputs["receipt"] = row
		}},
		{name: "dotdot-spool", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.SpoolObjectID = ".."
			c.LaneInputs["receipt"] = row
		}},
		{name: "unbounded-spool", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.SpoolObjectID = strings.Repeat("a", maxDurableOpaqueIDBytes+1)
			c.LaneInputs["receipt"] = row
		}},
		{name: "queued-has-intent", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.TargetTurnID, row.DispatchAttempt = "turn", "attempt"
			c.LaneInputs["receipt"] = row
		}},
		{name: "dispatching-misses-intent", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.State, row.Revision, row.UpdatedAt = ReceiptDispatching, 2, 11
			c.LaneInputs["receipt"] = row
		}},
		{name: "injected-wrong-native-session", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.State, row.TargetTurnID, row.DispatchAttempt, row.Revision, row.UpdatedAt = ReceiptInjected, "turn", "attempt", 2, 11
			row.NativeAcceptance = &NativeAcceptanceRef{NativeSessionID: "wrong", NativeMessageID: "message", AcceptedAt: 11}
			c.LaneInputs["receipt"] = row
		}},
		{name: "ambiguous-raw-detail", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.State, row.TargetTurnID, row.DispatchAttempt, row.Revision, row.UpdatedAt = ReceiptAmbiguous, "turn", "attempt", 2, 11
			row.AmbiguityCause = "Bearer secret raw detail"
			c.LaneInputs["receipt"] = row
		}},
		{name: "ambiguous-unknown-category", mutate: func(c *Catalog) {
			row := c.LaneInputs["receipt"]
			row.State, row.TargetTurnID, row.DispatchAttempt, row.Revision, row.UpdatedAt = ReceiptAmbiguous, "turn", "attempt", 2, 11
			row.AmbiguityCause = "some-new-category"
			c.LaneInputs["receipt"] = row
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog, _ := validNewDomainCatalog(t)
			test.mutate(&catalog)
			if err := validateCatalog(catalog); err == nil {
				t.Fatal("invalid receipt was accepted")
			}
		})
	}
}

func TestNewDurableRecordsCannotBypassInitialStates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Catalog, NativeSessionLeaseKey)
	}{
		{name: "receipt-injected", mutate: func(c *Catalog, _ NativeSessionLeaseKey) {
			row := c.LaneInputs["receipt"]
			row.State, row.TargetTurnID, row.DispatchAttempt, row.UpdatedAt = ReceiptInjected, "turn", "attempt", 11
			row.NativeAcceptance = &NativeAcceptanceRef{NativeSessionID: "lane-native", NativeMessageID: "message", AcceptedAt: 11}
			c.LaneInputs["receipt"] = row
		}},
		{name: "lease-held", mutate: func(c *Catalog, key NativeSessionLeaseKey) {
			row := c.NativeLeases[key]
			row.State, row.ProcessGroup = LeaseHeld, procinfo.Identity{PID: 44, Start: "lease", StrongStart: "lease-strong"}
			c.NativeLeases[key] = row
		}},
		{name: "binding-ready", mutate: func(c *Catalog, _ NativeSessionLeaseKey) {
			row := c.ComponentBindings["binding"]
			row.State = BindingReady
			c.ComponentBindings["binding"] = row
		}},
		{name: "session-idle", mutate: func(c *Catalog, _ NativeSessionLeaseKey) {
			row := c.ComponentSessions["attachment-fresh"]
			row.State = ComponentSessionIdle
			c.ComponentSessions["attachment-fresh"] = row
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := OpenState(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			catalog, key := validNewDomainCatalog(t)
			test.mutate(&catalog, key)
			if err := validateCatalog(catalog); err != nil {
				t.Fatalf("test record does not isolate insertion-state validation: %v", err)
			}
			if _, err := store.Commit(0, catalog); err == nil {
				t.Fatal("non-initial durable record was accepted")
			}
		})
	}
}

func TestReceiptTransitionsPreserveIntentEvidenceAndAppendOnlyOrder(t *testing.T) {
	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := validNewDomainCatalog(t)
	receipt := base.LaneInputs["receipt"]
	receipt.Sequence = 10
	base.LaneInputs["receipt"] = receipt
	lane := base.Lanes["lane"]
	lane.InputSequence = 10
	base.Lanes["lane"] = lane
	first, err := store.Commit(0, base)
	if err != nil {
		t.Fatal(err)
	}

	invalidAppend := first.Catalog
	low := invalidAppend.LaneInputs["receipt"]
	low.ReceiptID, low.SpoolObjectID, low.Sequence = "receipt-low", "spool-low", 9
	invalidAppend.LaneInputs["receipt-low"] = low
	if _, err := store.Commit(first.Revision, invalidAppend); err == nil {
		t.Fatal("new receipt with a lower lane-local sequence was accepted")
	}
	first, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}

	dispatch := first.Catalog
	receipt = dispatch.LaneInputs["receipt"]
	receipt.State, receipt.TargetTurnID, receipt.DispatchAttempt, receipt.Revision, receipt.UpdatedAt = ReceiptDispatching, "turn", "attempt", 2, 11
	dispatch.LaneInputs["receipt"] = receipt
	second, err := store.Commit(first.Revision, dispatch)
	if err != nil {
		t.Fatal(err)
	}

	mutatedAttempt := second.Catalog
	receipt = mutatedAttempt.LaneInputs["receipt"]
	receipt.DispatchAttempt, receipt.Revision, receipt.UpdatedAt = "attempt-other", 3, 12
	mutatedAttempt.LaneInputs["receipt"] = receipt
	if _, err := store.Commit(second.Revision, mutatedAttempt); err == nil {
		t.Fatal("stable dispatch attempt was rewritten")
	}
	second, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}

	ambiguous := second.Catalog
	receipt = ambiguous.LaneInputs["receipt"]
	receipt.State, receipt.AmbiguityCause, receipt.Revision, receipt.UpdatedAt = ReceiptAmbiguous, AmbiguityNativeAcceptanceUnproven, 3, 12
	ambiguous.LaneInputs["receipt"] = receipt
	third, err := store.Commit(second.Revision, ambiguous)
	if err != nil {
		t.Fatal(err)
	}
	proven := third.Catalog
	receipt = proven.LaneInputs["receipt"]
	receipt.State, receipt.AmbiguityCause, receipt.Revision, receipt.UpdatedAt = ReceiptInjected, "", 4, 13
	receipt.NativeAcceptance = &NativeAcceptanceRef{NativeSessionID: "lane-native", NativeMessageID: "native-operation", AcceptedAt: 13}
	proven.LaneInputs["receipt"] = receipt
	fourth, err := store.Commit(third.Revision, proven)
	if err != nil {
		t.Fatalf("authoritatively proven ambiguous receipt was not injectable: %v", err)
	}

	mutatedAcceptance := fourth.Catalog
	receipt = mutatedAcceptance.LaneInputs["receipt"]
	receipt.NativeAcceptance.NativeMessageID = "different-operation"
	receipt.Revision, receipt.UpdatedAt = 5, 14
	mutatedAcceptance.LaneInputs["receipt"] = receipt
	if _, err := store.Commit(fourth.Revision, mutatedAcceptance); err == nil {
		t.Fatal("durable native acceptance was rewritten")
	}
	fourth, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	retiring := fourth.Catalog
	receipt = retiring.LaneInputs["receipt"]
	receipt.State, receipt.Revision, receipt.UpdatedAt = ReceiptRetired, 5, 14
	retiring.LaneInputs["receipt"] = receipt
	fifth, err := store.Commit(fourth.Revision, retiring)
	if err != nil {
		t.Fatal(err)
	}
	removed := fifth.Catalog
	delete(removed.LaneInputs, "receipt")
	sixth, err := store.Commit(fifth.Revision, removed)
	if err != nil {
		t.Fatal(err)
	}
	reused := sixth.Catalog
	reused.LaneInputs["receipt-reused"] = LaneInputReceipt{
		Schema: LaneInputReceiptRecordSchema, ReceiptID: "receipt-reused", LaneID: "lane", Sequence: 10,
		Digest: sha256.Sum256([]byte("new")), Bytes: 3, SpoolObjectID: "spool-reused",
		State: ReceiptQueued, Revision: 1, AcceptedAt: 15, UpdatedAt: 15,
	}
	if _, err := store.Commit(sixth.Revision, reused); err == nil {
		t.Fatal("retired receipt sequence was reused after metadata removal")
	}
}

func TestNativeSessionLeaseRequiresExactOwnerAndMonotonicRecoveryEvidence(t *testing.T) {
	catalog, key := validNewDomainCatalog(t)
	lane := catalog.Lanes["lane"]
	lane.NativeSessionID = "different-native"
	catalog.Lanes["lane"] = lane
	if err := validateCatalog(catalog); err == nil {
		t.Fatal("lease owner with a different native session was accepted")
	}

	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	base, key := validNewDomainCatalog(t)
	first, err := store.Commit(0, base)
	if err != nil {
		t.Fatal(err)
	}
	held := first.Catalog
	lease := held.NativeLeases[key]
	lease.State, lease.ProcessGroup, lease.Revision, lease.UpdatedAt = LeaseHeld, procinfo.Identity{PID: 44, Start: "lease", StrongStart: "lease-strong"}, 2, 11
	held.NativeLeases[key] = lease
	second, err := store.Commit(first.Revision, held)
	if err != nil {
		t.Fatal(err)
	}

	changedProcess := second.Catalog
	lease = changedProcess.NativeLeases[key]
	lease.ProcessGroup, lease.Revision, lease.UpdatedAt = procinfo.Identity{PID: 45, Start: "other", StrongStart: "other-strong"}, 3, 12
	changedProcess.NativeLeases[key] = lease
	if _, err := store.Commit(second.Revision, changedProcess); err == nil {
		t.Fatal("held lease process identity changed without a new generation")
	}
	second, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}

	regressed := second.Catalog
	lease = regressed.NativeLeases[key]
	lease.Generation, lease.Revision, lease.UpdatedAt = 6, 3, 12
	regressed.NativeLeases[key] = lease
	if _, err := store.Commit(second.Revision, regressed); err == nil {
		t.Fatal("lease generation regression was accepted")
	}
}

func TestComponentRecordsRequireExactIdentityUniquenessAndMonotonicSequences(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Catalog)
	}{
		{name: "missing-strong-start", mutate: func(c *Catalog) {
			row := c.ComponentBindings["binding"]
			row.ProcessIdentity.StrongStart = ""
			c.ComponentBindings["binding"] = row
		}},
		{name: "missing-bootstrap-revision", mutate: func(c *Catalog) {
			row := c.ComponentBindings["binding"]
			row.BootstrapRevision = 0
			c.ComponentBindings["binding"] = row
		}},
		{name: "active-stale-generation", mutate: func(c *Catalog) {
			row := c.ComponentBindings["binding"]
			row.Generation = 6
			c.ComponentBindings["binding"] = row
		}},
		{name: "active-binding-detached", mutate: func(c *Catalog) {
			row := c.Attachments["attachment-fresh"]
			row.State = "detached"
			c.Attachments["attachment-fresh"] = row
		}},
		{name: "idle-session-without-native-authority", mutate: func(c *Catalog) {
			attachment := c.Attachments["attachment-fresh"]
			attachment.NativeSessionID = ""
			c.Attachments["attachment-fresh"] = attachment
			session := c.ComponentSessions["attachment-fresh"]
			session.State = ComponentSessionIdle
			c.ComponentSessions["attachment-fresh"] = session
		}},
		{name: "multiple-active-bindings", mutate: func(c *Catalog) {
			first := c.ComponentBindings["binding"]
			first.State = BindingReady
			c.ComponentBindings["binding"] = first
			second := first
			second.BindingID, second.BootstrapRevision = "binding-two", 2
			c.ComponentBindings["binding-two"] = second
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog, _ := validNewDomainCatalog(t)
			test.mutate(&catalog)
			if err := validateCatalog(catalog); err == nil {
				t.Fatal("invalid component authority was accepted")
			}
		})
	}

	store, err := OpenState(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := validNewDomainCatalog(t)
	first, err := store.Commit(0, base)
	if err != nil {
		t.Fatal(err)
	}
	advanced := first.Catalog
	binding := advanced.ComponentBindings["binding"]
	binding.LastInboundSeq, binding.LastOutboundSeq = 4, 5
	advanced.ComponentBindings["binding"] = binding
	session := advanced.ComponentSessions["attachment-fresh"]
	session.LastEventSeq = 6
	advanced.ComponentSessions["attachment-fresh"] = session
	second, err := store.Commit(first.Revision, advanced)
	if err != nil {
		t.Fatal(err)
	}
	regressed := second.Catalog
	binding = regressed.ComponentBindings["binding"]
	binding.LastInboundSeq = 3
	regressed.ComponentBindings["binding"] = binding
	if _, err := store.Commit(second.Revision, regressed); err == nil {
		t.Fatal("component binding replay sequence regressed")
	}
	second, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	regressed = second.Catalog
	session = regressed.ComponentSessions["attachment-fresh"]
	session.LastEventSeq = 5
	regressed.ComponentSessions["attachment-fresh"] = session
	if _, err := store.Commit(second.Revision, regressed); err == nil {
		t.Fatal("component session event sequence regressed")
	}
	second, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	changedBootstrap := second.Catalog
	binding = changedBootstrap.ComponentBindings["binding"]
	binding.BootstrapRevision = 2
	changedBootstrap.ComponentBindings["binding"] = binding
	if _, err := store.Commit(second.Revision, changedBootstrap); err == nil {
		t.Fatal("component bootstrap revision was rewritten")
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
