package componentruntime

import (
	"context"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestProductRuntimeAdaptersRecaptureOnEveryCallWithoutCachingEvidence(t *testing.T) {
	deps := productruntime.HostDeps{Generation: 17}
	attachment := daemon.ManagedAttachment{ID: "attachment", CatalogRevision: 9}
	peer := component.PeerEvidence{
		Peer:    localtransport.PeerIdentity{PID: 123, UID: 456},
		Process: procinfo.Identity{PID: 123, Start: "start", StrongStart: "strong"},
	}
	resolverCalls := 0
	resolver := productruntime.ComponentResolverFunc(func(
		_ context.Context,
		gotDeps productruntime.HostDeps,
		gotAttachment daemon.ManagedAttachment,
		gotPeer productruntime.ComponentPeerEvidence,
	) (productruntime.ComponentResolution, error) {
		resolverCalls++
		if gotDeps.Generation != deps.Generation || gotAttachment.ID != attachment.ID ||
			gotPeer.Peer != peer.Peer || gotPeer.Process != peer.Process {
			t.Fatalf("resolver inputs = deps=%+v attachment=%+v peer=%+v", gotDeps, gotAttachment, gotPeer)
		}
		return productruntime.ComponentResolution{
			BootstrapCapabilityID: "correlation",
			BootstrapRevision:     attachment.CatalogRevision,
			LiveEvidence: daemon.NativeEvidence{
				Process: peer.Process, Executable: "executable-" + string(rune('0'+resolverCalls)),
			},
		}, nil
	})
	adaptedResolver := AdaptProductResolver(deps, resolver)
	first, err := adaptedResolver.ResolveComponent(context.Background(), attachment, peer)
	if err != nil {
		t.Fatal(err)
	}
	second, err := adaptedResolver.ResolveComponent(context.Background(), attachment, peer)
	if err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 2 || first.LiveEvidence.Executable == second.LiveEvidence.Executable ||
		first.BootstrapRevision != attachment.CatalogRevision || second.BootstrapCapabilityID != "correlation" {
		t.Fatalf("fresh adapted resolutions = first=%+v second=%+v calls=%d", first, second, resolverCalls)
	}

	rebinderCalls := 0
	rebinder := productruntime.ComponentSessionRebinderFunc(func(
		_ context.Context,
		gotDeps productruntime.HostDeps,
		gotAttachment daemon.ManagedAttachment,
		oldID string,
		newID string,
		evidence []byte,
	) (daemon.NativeEvidence, error) {
		rebinderCalls++
		if gotDeps.Generation != deps.Generation || gotAttachment.ID != attachment.ID || oldID != "old" || newID != "new" || !reflect.DeepEqual(evidence, []byte("proof")) {
			t.Fatalf("rebinder inputs = deps=%+v attachment=%+v old=%q new=%q evidence=%q", gotDeps, gotAttachment, oldID, newID, evidence)
		}
		return daemon.NativeEvidence{Process: peer.Process, Executable: "rebind-" + string(rune('0'+rebinderCalls))}, nil
	})
	adaptedRebinder := AdaptProductSessionRebinder(deps, rebinder)
	firstLive, err := adaptedRebinder.ReattestSessionRebind(context.Background(), attachment, "old", "new", []byte("proof"))
	if err != nil {
		t.Fatal(err)
	}
	secondLive, err := adaptedRebinder.ReattestSessionRebind(context.Background(), attachment, "old", "new", []byte("proof"))
	if err != nil {
		t.Fatal(err)
	}
	if rebinderCalls != 2 || firstLive.Executable == secondLive.Executable {
		t.Fatalf("fresh adapted rebind evidence = first=%+v second=%+v calls=%d", firstLive, secondLive, rebinderCalls)
	}

	if AdaptProductResolver(deps, nil) != nil || AdaptProductSessionRebinder(deps, nil) != nil {
		t.Fatal("nil optional component seam became non-nil")
	}
}
