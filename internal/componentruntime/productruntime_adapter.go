package componentruntime

import (
	"context"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productruntime"
)

// AdaptProductResolver binds product-neutral host capabilities to a runtime
// product resolver. Every authority call still reaches the product resolver,
// which must perform a fresh live capture; this adapter keeps no evidence.
func AdaptProductResolver(deps productruntime.HostDeps, resolver productruntime.ComponentResolver) Resolver {
	if resolver == nil {
		return nil
	}
	return ResolverFunc(func(
		ctx context.Context,
		attachment daemon.ManagedAttachment,
		peer component.PeerEvidence,
	) (Resolution, error) {
		resolved, err := resolver.ResolveComponent(ctx, deps, attachment, productruntime.ComponentPeerEvidence{
			Peer: peer.Peer, Process: peer.Process,
		})
		if err != nil {
			return Resolution{}, err
		}
		return Resolution{
			BootstrapCapabilityID: resolved.BootstrapCapabilityID,
			BootstrapRevision:     resolved.BootstrapRevision,
			LiveEvidence:          resolved.LiveEvidence,
		}, nil
	})
}

// AdaptProductSessionRebinder binds product-neutral host capabilities to a
// runtime product re-attester without caching any returned live evidence.
func AdaptProductSessionRebinder(
	deps productruntime.HostDeps,
	rebinder productruntime.ComponentSessionRebinder,
) SessionRebinder {
	if rebinder == nil {
		return nil
	}
	return SessionRebinderFunc(func(
		ctx context.Context,
		attachment daemon.ManagedAttachment,
		oldNativeSessionID string,
		newNativeSessionID string,
		evidence []byte,
	) (daemon.NativeEvidence, error) {
		return rebinder.ReattestSessionRebind(
			ctx, deps, attachment, oldNativeSessionID, newNativeSessionID, evidence,
		)
	})
}
