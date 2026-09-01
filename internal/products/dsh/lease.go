package dsh

import (
	"context"
	"errors"
	"fmt"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

var ErrLeaseConflict = fmt.Errorf("%w: DSH native session already has an exact owner", productruntime.ErrAmbiguousSession)

type LeaseClaim struct {
	ProfileIdentity string
	NativeSessionID string
	LaneID          string
	Generation      uint64
	Process         productruntime.OwnedProcessRef
}

type LeaseAuthority interface {
	Acquire(context.Context, LeaseClaim) error
	Recover(context.Context, LeaseClaim) error
	Release(context.Context, LeaseClaim) error
}

// NativeLeaseEngine is the frozen daemon lease state machine used by the DSH
// adapter. The interface exists only to keep product tests isolated.
type NativeLeaseEngine interface {
	Acquire(daemon.NativeLeaseRequest) (daemon.NativeSessionLease, error)
	Hold(daemon.NativeLeaseRequest, procinfo.Identity) (daemon.NativeSessionLease, error)
	BeginRelease(daemon.NativeLeaseRequest) (daemon.NativeSessionLease, error)
	CompleteRelease(daemon.NativeLeaseRequest) (daemon.NativeSessionLease, error)
	Recover(daemon.NativeLeaseRequest, procinfo.Identity) (daemon.NativeSessionLease, error)
}

// PriorLeaseProcessLookup supplies the exact durable prior process token during
// daemon-generation recovery. It is daemon authority, not product-local state.
type PriorLeaseProcessLookup func(context.Context, LeaseClaim) (procinfo.Identity, error)

type NativeLeaseAuthority struct {
	engine      NativeLeaseEngine
	priorLookup PriorLeaseProcessLookup
}

func NewNativeLeaseAuthority(engine NativeLeaseEngine, priorLookup PriorLeaseProcessLookup) (*NativeLeaseAuthority, error) {
	if engine == nil {
		return nil, errors.New("DSH lease authority requires daemon native-lease engine")
	}
	return &NativeLeaseAuthority{engine: engine, priorLookup: priorLookup}, nil
}

func (authority *NativeLeaseAuthority) Acquire(_ context.Context, claim LeaseClaim) error {
	request, err := leaseRequest(claim)
	if err != nil {
		return err
	}
	acquired, err := authority.engine.Acquire(request)
	if err != nil {
		return mapLeaseError(err)
	}
	if acquired.State == daemon.LeaseHeld {
		if acquired.ProcessGroup == claim.Process.Process {
			return nil
		}
		return fmt.Errorf("%w: DSH lease is held by another exact process", ErrLeaseConflict)
	}
	if _, err := authority.engine.Hold(request, claim.Process.Process); err != nil {
		_, beginErr := authority.engine.BeginRelease(request)
		_, releaseErr := authority.engine.CompleteRelease(request)
		return errors.Join(mapLeaseError(err), mapLeaseError(beginErr), mapLeaseError(releaseErr))
	}
	return nil
}

func (authority *NativeLeaseAuthority) Recover(ctx context.Context, claim LeaseClaim) error {
	if authority.priorLookup == nil {
		return fmt.Errorf("%w: DSH recovery requires daemon prior-lease process lookup", productruntime.ErrUnsupportedRecovery)
	}
	request, err := leaseRequest(claim)
	if err != nil {
		return err
	}
	prior, err := authority.priorLookup(ctx, claim)
	if err != nil {
		return err
	}
	recovered, recoverErr := authority.engine.Recover(request, prior)
	if recoverErr != nil && !errors.Is(recoverErr, daemon.ErrNativeLeaseStale) {
		return mapLeaseError(recoverErr)
	}
	if recoverErr == nil {
		if recovered.ProcessGroup == claim.Process.Process {
			return nil
		}
		return fmt.Errorf("%w: prior DSH ACP owner is still live", ErrLeaseConflict)
	}
	// A proven-dead prior owner is durably released before this new process is
	// allowed to resume the native session.
	return authority.Acquire(ctx, claim)
}

func (authority *NativeLeaseAuthority) Release(_ context.Context, claim LeaseClaim) error {
	request, err := leaseRequest(claim)
	if err != nil {
		return err
	}
	if _, err := authority.engine.BeginRelease(request); err != nil {
		// A prior call may have durably reached Releasing or CleanupDebt before
		// its completion failed. CompleteRelease is the frozen convergence step
		// for both states; attempting it after a failed Begin is safe because the
		// engine rejects every other state and exact owner.
		if _, completeErr := authority.engine.CompleteRelease(request); completeErr == nil {
			return nil
		} else {
			return errors.Join(mapLeaseError(err), mapLeaseError(completeErr))
		}
	}
	if _, err := authority.engine.CompleteRelease(request); err != nil {
		return mapLeaseError(err)
	}
	return nil
}

func leaseRequest(claim LeaseClaim) (daemon.NativeLeaseRequest, error) {
	if claim.ProfileIdentity == "" || claim.NativeSessionID == "" || claim.LaneID == "" || claim.Generation == 0 ||
		claim.Process.Process.PID <= 1 || claim.Process.Process.Start == "" || claim.Process.Process.StrongStart == "" || claim.Process.ProcessGroup <= 1 {
		return daemon.NativeLeaseRequest{}, fmt.Errorf("%w: DSH lease claim is incomplete", productruntime.ErrAmbiguousSession)
	}
	return daemon.NativeLeaseRequest{
		ProductID: ProductID, ProfileIdentity: claim.ProfileIdentity, NativeSessionID: claim.NativeSessionID,
		OwnerLaneID: claim.LaneID, Generation: claim.Generation,
	}, nil
}

func mapLeaseError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, daemon.ErrNativeLeaseConflict), errors.Is(err, daemon.ErrNativeLeaseLive),
		errors.Is(err, daemon.ErrNativeLeaseRecoveryRequired), errors.Is(err, daemon.ErrNativeLeaseReleaseRequired):
		return fmt.Errorf("%w: %v", ErrLeaseConflict, err)
	case errors.Is(err, daemon.ErrNativeLeaseCleanupDebt):
		return fmt.Errorf("%w: %v", productruntime.ErrCleanupDebt, err)
	default:
		return err
	}
}

var _ LeaseAuthority = (*NativeLeaseAuthority)(nil)
