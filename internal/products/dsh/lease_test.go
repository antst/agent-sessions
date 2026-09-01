package dsh

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type scriptedLeaseEngine struct {
	acquireResult daemon.NativeSessionLease
	acquireErr    error
	recoverResult daemon.NativeSessionLease
	recoverErr    error
	lastRequest   daemon.NativeLeaseRequest
	heldProcess   procinfo.Identity
	recoveredFrom procinfo.Identity
	beginErr      error
	completeErrs  []error
	beginCalls    int
	completeCalls int
}

func (engine *scriptedLeaseEngine) Acquire(request daemon.NativeLeaseRequest) (daemon.NativeSessionLease, error) {
	engine.lastRequest = request
	return engine.acquireResult, engine.acquireErr
}
func (engine *scriptedLeaseEngine) Hold(request daemon.NativeLeaseRequest, process procinfo.Identity) (daemon.NativeSessionLease, error) {
	engine.lastRequest, engine.heldProcess = request, process
	return daemon.NativeSessionLease{State: daemon.LeaseHeld, ProcessGroup: process}, nil
}
func (engine *scriptedLeaseEngine) BeginRelease(request daemon.NativeLeaseRequest) (daemon.NativeSessionLease, error) {
	engine.lastRequest = request
	engine.beginCalls++
	return daemon.NativeSessionLease{State: daemon.LeaseReleasing}, engine.beginErr
}
func (engine *scriptedLeaseEngine) CompleteRelease(request daemon.NativeLeaseRequest) (daemon.NativeSessionLease, error) {
	engine.lastRequest = request
	engine.completeCalls++
	if len(engine.completeErrs) > 0 {
		err := engine.completeErrs[0]
		engine.completeErrs = engine.completeErrs[1:]
		return daemon.NativeSessionLease{State: daemon.LeaseCleanupDebt}, err
	}
	return daemon.NativeSessionLease{State: daemon.LeaseReleased}, nil
}

func TestNativeLeaseReleaseConvergesFromReleasingAndCleanupDebt(t *testing.T) {
	process := procinfo.Identity{PID: 201, Start: "new", StrongStart: "new-strong"}
	engine := &scriptedLeaseEngine{
		beginErr:     daemon.ErrNativeLeaseReleaseRequired,
		completeErrs: []error{daemon.ErrNativeLeaseCleanupDebt, nil},
	}
	authority, err := NewNativeLeaseAuthority(engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Release(context.Background(), exactLeaseClaim(process)); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("first release error = %v, want ErrCleanupDebt", err)
	}
	if err := authority.Release(context.Background(), exactLeaseClaim(process)); err != nil {
		t.Fatalf("retry release: %v", err)
	}
	if engine.beginCalls != 2 || engine.completeCalls != 2 {
		t.Fatalf("release begin/complete calls = %d/%d", engine.beginCalls, engine.completeCalls)
	}
}
func (engine *scriptedLeaseEngine) Recover(request daemon.NativeLeaseRequest, process procinfo.Identity) (daemon.NativeSessionLease, error) {
	engine.lastRequest, engine.recoveredFrom = request, process
	return engine.recoverResult, engine.recoverErr
}

func exactLeaseClaim(process procinfo.Identity) LeaseClaim {
	return LeaseClaim{
		ProfileIdentity: "profile", NativeSessionID: "native", LaneID: "lane", Generation: 8,
		Process: productruntime.OwnedProcessRef{Process: process, ProcessGroup: process.PID},
	}
}

func TestNativeLeaseAuthorityHoldsExactProcessAndRejectsDifferentHeldOwner(t *testing.T) {
	process := procinfo.Identity{PID: 201, Start: "new", StrongStart: "new-strong"}
	engine := &scriptedLeaseEngine{acquireResult: daemon.NativeSessionLease{State: daemon.LeasePrepared}}
	authority, err := NewNativeLeaseAuthority(engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Acquire(context.Background(), exactLeaseClaim(process)); err != nil {
		t.Fatal(err)
	}
	if engine.heldProcess != process || engine.lastRequest.ProductID != ProductID || engine.lastRequest.NativeSessionID != "native" {
		t.Fatalf("held process/request = %+v / %+v", engine.heldProcess, engine.lastRequest)
	}

	prior := procinfo.Identity{PID: 101, Start: "old", StrongStart: "old-strong"}
	engine.acquireResult = daemon.NativeSessionLease{State: daemon.LeaseHeld, ProcessGroup: prior}
	if err := authority.Acquire(context.Background(), exactLeaseClaim(process)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("different held process error = %v, want ErrLeaseConflict", err)
	}
}

func TestNativeLeaseAuthorityRecoveryRequiresPriorExactDeathBeforeNewHold(t *testing.T) {
	prior := procinfo.Identity{PID: 101, Start: "old", StrongStart: "old-strong"}
	process := procinfo.Identity{PID: 201, Start: "new", StrongStart: "new-strong"}
	engine := &scriptedLeaseEngine{
		recoverErr:    daemon.ErrNativeLeaseStale,
		acquireResult: daemon.NativeSessionLease{State: daemon.LeasePrepared},
	}
	authority, err := NewNativeLeaseAuthority(engine, func(context.Context, LeaseClaim) (procinfo.Identity, error) { return prior, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Recover(context.Background(), exactLeaseClaim(process)); err != nil {
		t.Fatal(err)
	}
	if engine.recoveredFrom != prior || engine.heldProcess != process {
		t.Fatalf("recovered prior/held new = %+v / %+v", engine.recoveredFrom, engine.heldProcess)
	}

	live := &scriptedLeaseEngine{recoverResult: daemon.NativeSessionLease{State: daemon.LeaseHeld, ProcessGroup: prior}}
	authority, _ = NewNativeLeaseAuthority(live, func(context.Context, LeaseClaim) (procinfo.Identity, error) { return prior, nil })
	if err := authority.Recover(context.Background(), exactLeaseClaim(process)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("live prior recovery error = %v, want ErrLeaseConflict", err)
	}
}
