package productruntime

import (
	"context"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
)

// ComponentPeerTransport is the catalog transport key for a managed local
// component-broker peer. It is data-only and does not select an implementation
// through package initialization.
const ComponentPeerTransport = "component"

// ComponentPeerEvidence is the product-neutral live peer evidence presented
// to a component resolver. The kernel peer and independently captured process
// identity must describe the same live process.
type ComponentPeerEvidence struct {
	Peer    localtransport.PeerIdentity
	Process procinfo.Identity
}

// ComponentResolution is fresh, secret-free component evidence. The
// capability ID is optional correlation metadata and never grants authority;
// the bootstrap value is authenticated independently against the durable hash.
type ComponentResolution struct {
	BootstrapCapabilityID string
	BootstrapRevision     uint64
	LiveEvidence          daemon.NativeEvidence
}

// ComponentResolver freshly re-captures process, executable, artifact, tuple,
// and ancestry evidence on every call. Implementations must use the supplied
// live HostDeps and must never return a stored identity as authorization.
type ComponentResolver interface {
	ResolveComponent(context.Context, HostDeps, daemon.ManagedAttachment, ComponentPeerEvidence) (ComponentResolution, error)
}

// ComponentResolverFunc adapts a product-owned live resolver.
type ComponentResolverFunc func(
	context.Context,
	HostDeps,
	daemon.ManagedAttachment,
	ComponentPeerEvidence,
) (ComponentResolution, error)

func (f ComponentResolverFunc) ResolveComponent(
	ctx context.Context,
	deps HostDeps,
	attachment daemon.ManagedAttachment,
	peer ComponentPeerEvidence,
) (ComponentResolution, error) {
	return f(ctx, deps, attachment, peer)
}

// ComponentSessionRebinder freshly re-captures product tuple, artifact, and
// process evidence on every session.rebind. Stored or cached identity is never
// authority.
type ComponentSessionRebinder interface {
	ReattestSessionRebind(
		context.Context,
		HostDeps,
		daemon.ManagedAttachment,
		string,
		string,
		[]byte,
	) (daemon.NativeEvidence, error)
}

// ComponentSessionRebinderFunc adapts a product-owned live re-attester.
type ComponentSessionRebinderFunc func(
	context.Context,
	HostDeps,
	daemon.ManagedAttachment,
	string,
	string,
	[]byte,
) (daemon.NativeEvidence, error)

func (f ComponentSessionRebinderFunc) ReattestSessionRebind(
	ctx context.Context,
	deps HostDeps,
	attachment daemon.ManagedAttachment,
	oldNativeSessionID string,
	newNativeSessionID string,
	evidence []byte,
) (daemon.NativeEvidence, error) {
	return f(ctx, deps, attachment, oldNativeSessionID, newNativeSessionID, evidence)
}
