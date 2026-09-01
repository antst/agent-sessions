package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func newNativeLeaseTestEngine(t *testing.T) (*NativeLeaseEngine, *StateStore) {
	t.Helper()
	store, err := OpenState(filepath.Join(t.TempDir(), "state"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	catalog := emptyCatalog()
	catalog.Host = HostRuntime{User: "1000", Host: "test", Generation: 7}
	catalog.Lanes["lane"] = Lane{ID: "lane", Product: "codex", ProfileIdentity: "profile", NativeSessionID: "native", State: "running"}
	catalog.Lanes["other"] = Lane{ID: "other", Product: "codex", ProfileIdentity: "profile", NativeSessionID: "native", State: "running"}
	if _, err := store.Commit(0, catalog); err != nil {
		t.Fatal(err)
	}
	engine, err := NewNativeLeaseEngine(store, 7)
	if err != nil {
		t.Fatal(err)
	}
	engine.now = func() time.Time { return time.Unix(100, 0) }
	return engine, store
}

func leaseRequest(owner string) NativeLeaseRequest {
	return NativeLeaseRequest{ProductID: "codex", ProfileIdentity: "profile", NativeSessionID: "native", OwnerLaneID: owner, Generation: 7}
}

func TestNativeLeaseAcquireHoldAndExactCompositeConflict(t *testing.T) {
	engine, _ := newNativeLeaseTestEngine(t)
	lease, err := engine.Acquire(leaseRequest("lane"))
	if err != nil || lease.State != LeasePrepared {
		t.Fatalf("acquire=%+v err=%v", lease, err)
	}
	if same, err := engine.Acquire(leaseRequest("lane")); err != nil || same.State != LeasePrepared || same.Revision != lease.Revision {
		t.Fatalf("prepared idempotent acquire=%+v err=%v", same, err)
	}
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	held, err := engine.Hold(leaseRequest("lane"), process)
	if err != nil || held.State != LeaseHeld || held.ProcessGroup != process {
		t.Fatalf("hold=%+v err=%v", held, err)
	}
	if same, err := engine.Acquire(leaseRequest("lane")); err != nil || same.State != LeaseHeld || same.ProcessGroup != process {
		t.Fatalf("held idempotent acquire=%+v err=%v", same, err)
	}
	if _, err := engine.Acquire(leaseRequest("other")); !errors.Is(err, ErrNativeLeaseConflict) {
		t.Fatalf("conflicting owner error=%v", err)
	}
	key, _ := NewNativeSessionLeaseKey("codex", "profile", "native")
	if key != NativeSessionLeaseKey(`["codex","profile","native"]`) {
		t.Fatalf("unexpected composite key %q", key)
	}
}

func TestNativeLeaseReleaseRequiresExactProofOfDeath(t *testing.T) {
	engine, store := newNativeLeaseTestEngine(t)
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	_, _ = engine.Acquire(leaseRequest("lane"))
	_, _ = engine.Hold(leaseRequest("lane"), process)
	if _, err := engine.BeginRelease(leaseRequest("lane")); err != nil {
		t.Fatal(err)
	}
	engine.observe = func(procinfo.Identity) procinfo.IdentityObservation {
		return procinfo.IdentityObservation{Status: procinfo.IdentityMatches, Current: process}
	}
	if _, err := engine.CompleteRelease(leaseRequest("lane")); !errors.Is(err, ErrNativeLeaseLive) {
		t.Fatalf("live release error=%v", err)
	}
	engine.observe = func(procinfo.Identity) procinfo.IdentityObservation {
		return procinfo.IdentityObservation{Status: procinfo.IdentityStale}
	}
	released, err := engine.CompleteRelease(leaseRequest("lane"))
	if err != nil || released.State != LeaseReleased {
		t.Fatalf("released=%+v err=%v", released, err)
	}
	snapshot, _ := store.Read()
	key, _ := NewNativeSessionLeaseKey("codex", "profile", "native")
	if snapshot.Catalog.NativeLeases[key].State != LeaseReleased {
		t.Fatalf("release not durable: %+v", snapshot.Catalog.NativeLeases[key])
	}
}

func TestNativeLeaseUnknownObservationBecomesCleanupDebt(t *testing.T) {
	engine, _ := newNativeLeaseTestEngine(t)
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	_, _ = engine.Acquire(leaseRequest("lane"))
	_, _ = engine.Hold(leaseRequest("lane"), process)
	_, _ = engine.BeginRelease(leaseRequest("lane"))
	engine.observe = func(procinfo.Identity) procinfo.IdentityObservation {
		return procinfo.IdentityObservation{Status: procinfo.IdentityUnknown}
	}
	lease, err := engine.CompleteRelease(leaseRequest("lane"))
	if !errors.Is(err, ErrNativeLeaseCleanupDebt) || lease.State != LeaseCleanupDebt {
		t.Fatalf("unknown release=%+v err=%v", lease, err)
	}
}

func TestNativeLeaseRecoveryReattachesOnlyExactLiveOwner(t *testing.T) {
	engine, store := newNativeLeaseTestEngine(t)
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	_, _ = engine.Acquire(leaseRequest("lane"))
	_, _ = engine.Hold(leaseRequest("lane"), process)
	snapshot, _ := store.Read()
	catalog := snapshot.Catalog
	catalog.Host.Generation = 8
	if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	engine.generation = 8
	engine.observe = func(got procinfo.Identity) procinfo.IdentityObservation {
		if got != process {
			t.Fatalf("observed wrong process: %+v", got)
		}
		return procinfo.IdentityObservation{Status: procinfo.IdentityMatches, Current: process}
	}
	recovered, err := engine.Recover(leaseRequest("lane"), process)
	if err != nil || recovered.Generation != 8 || recovered.State != LeaseHeld || recovered.OwnerLaneID != "lane" {
		t.Fatalf("recover=%+v err=%v", recovered, err)
	}
	if _, err := engine.Recover(leaseRequest("other"), process); !errors.Is(err, ErrNativeLeaseConflict) {
		t.Fatalf("wrong-owner recover error=%v", err)
	}
}

func TestNativeLeaseRecoveryProvenDeadReleasesBeforeReacquire(t *testing.T) {
	engine, store := newNativeLeaseTestEngine(t)
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	_, _ = engine.Acquire(leaseRequest("lane"))
	_, _ = engine.Hold(leaseRequest("lane"), process)
	snapshot, _ := store.Read()
	catalog := snapshot.Catalog
	catalog.Host.Generation = 8
	if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	engine.generation = 8
	engine.observe = func(procinfo.Identity) procinfo.IdentityObservation {
		return procinfo.IdentityObservation{Status: procinfo.IdentityStale}
	}
	released, err := engine.Recover(leaseRequest("lane"), process)
	if !errors.Is(err, ErrNativeLeaseStale) || released.State != LeaseReleased {
		t.Fatalf("stale recover=%+v err=%v", released, err)
	}
	request := leaseRequest("lane")
	request.Generation = 8
	acquired, err := engine.Acquire(request)
	if err != nil || acquired.State != LeasePrepared || acquired.Generation != 8 || acquired.Revision != 1 {
		t.Fatalf("reacquire=%+v err=%v", acquired, err)
	}
}

func TestNativeLeaseReleasedTupleMayChangeOwnerOnlyAfterFreshDeathProof(t *testing.T) {
	engine, _ := newNativeLeaseTestEngine(t)
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	_, _ = engine.Acquire(leaseRequest("lane"))
	_, _ = engine.Hold(leaseRequest("lane"), process)
	_, _ = engine.BeginRelease(leaseRequest("lane"))
	engine.observe = func(procinfo.Identity) procinfo.IdentityObservation {
		return procinfo.IdentityObservation{Status: procinfo.IdentityStale}
	}
	if _, err := engine.CompleteRelease(leaseRequest("lane")); err != nil {
		t.Fatal(err)
	}
	reassigned, err := engine.Acquire(leaseRequest("other"))
	if err != nil || reassigned.OwnerLaneID != "other" || reassigned.State != LeasePrepared {
		t.Fatalf("reassigned=%+v err=%v", reassigned, err)
	}
}

func TestNativeLeaseAcquireRequiresRecoveryAcrossGeneration(t *testing.T) {
	engine, store := newNativeLeaseTestEngine(t)
	process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	_, _ = engine.Acquire(leaseRequest("lane"))
	_, _ = engine.Hold(leaseRequest("lane"), process)
	snapshot, _ := store.Read()
	catalog := snapshot.Catalog
	catalog.Host.Generation = 8
	if _, err := store.Commit(snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	engine.generation = 8
	request := leaseRequest("lane")
	request.Generation = 8
	if _, err := engine.Acquire(request); !errors.Is(err, ErrNativeLeaseRecoveryRequired) {
		t.Fatalf("stale-generation acquire error=%v", err)
	}
}

func TestNativeLeaseAcquireDoesNotResurrectReleasingOrCleanupDebt(t *testing.T) {
	t.Run("releasing", func(t *testing.T) {
		engine, store := newNativeLeaseTestEngine(t)
		process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
		_, _ = engine.Acquire(leaseRequest("lane"))
		_, _ = engine.Hold(leaseRequest("lane"), process)
		_, _ = engine.BeginRelease(leaseRequest("lane"))
		if _, err := engine.Acquire(leaseRequest("lane")); !errors.Is(err, ErrNativeLeaseReleaseRequired) {
			t.Fatalf("releasing acquire error=%v", err)
		}
		snapshot, _ := store.Read()
		key, _ := NewNativeSessionLeaseKey("codex", "profile", "native")
		if snapshot.Catalog.NativeLeases[key].State != LeaseReleasing {
			t.Fatalf("acquire changed releasing lease: %+v", snapshot.Catalog.NativeLeases[key])
		}
	})

	t.Run("cleanup-debt", func(t *testing.T) {
		engine, store := newNativeLeaseTestEngine(t)
		process := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
		_, _ = engine.Acquire(leaseRequest("lane"))
		_, _ = engine.Hold(leaseRequest("lane"), process)
		_, _ = engine.BeginRelease(leaseRequest("lane"))
		engine.observe = func(procinfo.Identity) procinfo.IdentityObservation {
			return procinfo.IdentityObservation{Status: procinfo.IdentityUnknown}
		}
		if _, err := engine.CompleteRelease(leaseRequest("lane")); !errors.Is(err, ErrNativeLeaseCleanupDebt) {
			t.Fatalf("cleanup transition error=%v", err)
		}
		if _, err := engine.Acquire(leaseRequest("lane")); !errors.Is(err, ErrNativeLeaseCleanupDebt) {
			t.Fatalf("cleanup-debt acquire error=%v", err)
		}
		snapshot, _ := store.Read()
		key, _ := NewNativeSessionLeaseKey("codex", "profile", "native")
		if snapshot.Catalog.NativeLeases[key].State != LeaseCleanupDebt {
			t.Fatalf("acquire changed cleanup-debt lease: %+v", snapshot.Catalog.NativeLeases[key])
		}
	})
}
