// Package componentruntime joins the component protocol to the daemon's
// durable catalog without making either package depend on the other.
package componentruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/statestore"
)

const bindingIDPrefix = "component-binding-"

// Resolution is fresh, secret-free evidence for one prepared component
// launch. BootstrapCapabilityID is optional correlation metadata and never
// grants authority. The authority independently checks the presented value
// against the durable capability hash, plus the returned revision, process
// identity, and exact durable NativeEvidence.
type Resolution struct {
	BootstrapCapabilityID string
	BootstrapRevision     uint64
	LiveEvidence          daemon.NativeEvidence
}

// Resolver re-captures one component peer against an exact durable attachment.
// It must not cache authorization across calls: Ready-loss and reconnect replay
// are freshly resolved every time.
type Resolver interface {
	ResolveComponent(context.Context, daemon.ManagedAttachment, component.PeerEvidence) (Resolution, error)
}

// ResolverFunc adapts one narrow component evidence resolver.
type ResolverFunc func(context.Context, daemon.ManagedAttachment, component.PeerEvidence) (Resolution, error)

func (f ResolverFunc) ResolveComponent(
	ctx context.Context,
	attachment daemon.ManagedAttachment,
	peer component.PeerEvidence,
) (Resolution, error) {
	return f(ctx, attachment, peer)
}

// CatalogAdmitter performs coordinator-owned durable work in the same catalog
// CAS that adopts a binding. Product drivers never receive this authority.
type CatalogAdmitter interface {
	AdmitComponentFrame(context.Context, *daemon.Catalog, component.BindingView, component.Frame) error
}

// CatalogAdmitterFunc adapts a durable catalog mutation.
type CatalogAdmitterFunc func(context.Context, *daemon.Catalog, component.BindingView, component.Frame) error

func (f CatalogAdmitterFunc) AdmitComponentFrame(
	ctx context.Context,
	catalog *daemon.Catalog,
	binding component.BindingView,
	frame component.Frame,
) error {
	return f(ctx, catalog, binding, frame)
}

// SessionRebinder freshly corroborates a protocol session.rebind before the
// coordinator changes the attachment-to-native-session relationship. The
// returned evidence is secret-free and becomes the new durable live evidence.
type SessionRebinder interface {
	ReattestSessionRebind(
		context.Context,
		daemon.ManagedAttachment,
		string,
		string,
		[]byte,
	) (daemon.NativeEvidence, error)
}

// SessionRebinderFunc adapts a product-owned session re-attester.
type SessionRebinderFunc func(
	context.Context,
	daemon.ManagedAttachment,
	string,
	string,
	[]byte,
) (daemon.NativeEvidence, error)

func (f SessionRebinderFunc) ReattestSessionRebind(
	ctx context.Context,
	attachment daemon.ManagedAttachment,
	oldNativeSessionID string,
	newNativeSessionID string,
	evidence []byte,
) (daemon.NativeEvidence, error) {
	return f(ctx, attachment, oldNativeSessionID, newNativeSessionID, evidence)
}

// IDGenerator returns a secret-free durable component binding identifier.
type IDGenerator func() (string, error)

// Config composes one generation's durable component authority.
type Config struct {
	Store      *daemon.StateStore
	Generation uint64
	Resolver   Resolver
	Admitter   CatalogAdmitter
	Rebinder   SessionRebinder
	NewID      IDGenerator

	// Test hooks model committed crash boundaries. They are deliberately
	// unexported and have no production behavior.
	afterClosedSessionDelete func() error
	afterAdoptionCommit      func() error
	afterRebindClose         func() error
	afterRebindAttachment    func() error
}

// Authority implements both component.Authorizer and component.Handler over
// the real daemon StateStore. A broker can use the same value for both roles.
type Authority struct {
	store      *daemon.StateStore
	generation uint64
	resolver   Resolver
	admitter   CatalogAdmitter
	rebinder   SessionRebinder
	newID      IDGenerator

	afterClosedSessionDelete func() error
	afterAdoptionCommit      func() error
	afterRebindClose         func() error
	afterRebindAttachment    func() error
}

var (
	_ component.Authorizer = (*Authority)(nil)
	_ component.Handler    = (*Authority)(nil)
)

// New constructs one generation-scoped authority and converges bounded closed
// binding tombstones left by a crash after adoption.
func New(config Config) (*Authority, error) {
	if config.Store == nil {
		return nil, errors.New("component authority state store is required")
	}
	if config.Generation == 0 {
		return nil, errors.New("component authority generation must be positive")
	}
	if config.Resolver == nil {
		return nil, errors.New("component attachment resolver is required")
	}
	if config.NewID == nil {
		config.NewID = randomBindingID
	}
	authority := &Authority{
		store: config.Store, generation: config.Generation, resolver: config.Resolver,
		admitter: config.Admitter, rebinder: config.Rebinder, newID: config.NewID,
		afterClosedSessionDelete: config.afterClosedSessionDelete,
		afterAdoptionCommit:      config.afterAdoptionCommit,
		afterRebindClose:         config.afterRebindClose,
		afterRebindAttachment:    config.afterRebindAttachment,
	}
	snapshot, err := authority.store.Read()
	if err != nil {
		return nil, err
	}
	if snapshot.Catalog.Host.Generation != config.Generation {
		return nil, errors.New("component authority generation does not match durable host")
	}
	if err := authority.collectClosedBindings(context.Background()); err != nil {
		return nil, fmt.Errorf("collect component binding tombstones: %w", err)
	}
	return authority, nil
}

// Bootstrap consumes or exactly replays a one-time prepared launch. Within a
// generation an unadopted retry returns its exact BindingID. After a generation
// sweep, the Retiring anchor creates one fresh current-generation successor.
func (a *Authority) Bootstrap(
	ctx context.Context,
	claim component.BootstrapClaim,
	peer component.PeerEvidence,
) (component.Authorization, error) {
	for {
		if err := ctx.Err(); err != nil {
			return component.Authorization{}, err
		}
		snapshot, attachment, resolution, err := a.resolve(ctx, claim.AttachmentID, peer)
		if err != nil {
			return component.Authorization{}, err
		}
		if err := validateBootstrapClaim(claim, peer, attachment, resolution); err != nil {
			return component.Authorization{}, err
		}

		catalog := snapshot.Catalog
		current, predecessor, err := bootstrapRows(catalog, attachment.ID, resolution.BootstrapRevision, a.generation)
		if err != nil {
			return component.Authorization{}, err
		}
		if current != nil {
			if current.State != daemon.BindingBinding {
				return component.Authorization{}, replayError("bootstrap binding was already adopted")
			}
			if current.ProcessIdentity != peer.Process {
				return component.Authorization{}, unauthorizedError("bootstrap capability belongs to another process")
			}
			return authorization(*current, attachment, resolution), nil
		}
		if predecessor != nil {
			if predecessor.State != daemon.BindingRetiring || predecessor.Generation >= a.generation || predecessor.LastInboundSeq != 0 {
				return component.Authorization{}, replayError("bootstrap anchor is stale or already adopted")
			}
			if predecessor.ProcessIdentity != peer.Process {
				return component.Authorization{}, unauthorizedError("bootstrap capability belongs to another process")
			}
		}

		binding, err := a.newBinding(attachment.ID, resolution.BootstrapRevision, peer)
		if err != nil {
			return component.Authorization{}, err
		}
		if _, exists := catalog.ComponentBindings[binding.BindingID]; exists {
			continue
		}
		if predecessor != nil {
			closed := *predecessor
			closed.State = daemon.BindingClosed
			catalog.ComponentBindings[closed.BindingID] = closed
		}
		catalog.ComponentBindings[binding.BindingID] = binding
		if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
			if errors.Is(err, statestore.ErrConflict) {
				continue
			}
			return component.Authorization{}, err
		}
		return authorization(binding, attachment, resolution), nil
	}
}

// Reconnect freshly re-attests only the immediate adopted predecessor. It
// creates at most one unadopted successor for an attachment/generation and
// returns that exact successor after Ready loss.
func (a *Authority) Reconnect(
	ctx context.Context,
	claim component.ReconnectClaim,
	peer component.PeerEvidence,
) (component.Authorization, error) {
	for {
		if err := ctx.Err(); err != nil {
			return component.Authorization{}, err
		}
		snapshot, attachment, resolution, err := a.resolve(ctx, claim.AttachmentID, peer)
		if err != nil {
			return component.Authorization{}, err
		}
		catalog := snapshot.Catalog
		prior, ok := catalog.ComponentBindings[claim.PriorBindingID]
		if !ok || prior.AttachmentID != claim.AttachmentID {
			return component.Authorization{}, replayError("reconnect predecessor is unavailable")
		}
		if err := validateReconnectClaim(claim, peer, attachment, resolution, prior); err != nil {
			return component.Authorization{}, err
		}

		if prior.State == daemon.BindingReady {
			if prior.Generation != a.generation {
				return component.Authorization{}, replayError("stale ready binding escaped generation retirement")
			}
			prior.State = daemon.BindingRetiring
			catalog.ComponentBindings[prior.BindingID] = prior
			closeSessionForBinding(&catalog, prior.BindingID)
			if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
				if errors.Is(err, statestore.ErrConflict) {
					continue
				}
				return component.Authorization{}, err
			}
			continue
		}

		current, err := currentSuccessor(catalog, prior, a.generation)
		if err != nil {
			return component.Authorization{}, err
		}
		if current != nil {
			if current.State != daemon.BindingBinding || current.ProcessIdentity != peer.Process {
				return component.Authorization{}, replayError("reconnect successor was already adopted or conflicts")
			}
			return authorization(*current, attachment, resolution), nil
		}
		if prior.State != daemon.BindingRetiring {
			return component.Authorization{}, replayError("reconnect predecessor is fenced")
		}

		binding, err := a.newBinding(attachment.ID, prior.BootstrapRevision, peer)
		if err != nil {
			return component.Authorization{}, err
		}
		if _, exists := catalog.ComponentBindings[binding.BindingID]; exists {
			continue
		}
		prior.State = daemon.BindingClosed
		catalog.ComponentBindings[prior.BindingID] = prior
		catalog.ComponentBindings[binding.BindingID] = binding
		if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
			if errors.Is(err, statestore.ErrConflict) {
				continue
			}
			return component.Authorization{}, err
		}
		return authorization(binding, attachment, resolution), nil
	}
}

// HandleComponentFrame admits common session authority and adopts a binding in
// the same durable CAS. A Closed session row is removed in a separate commit
// before exact re-announcement so a crash at either boundary converges.
func (a *Authority) HandleComponentFrame(
	ctx context.Context,
	view component.BindingView,
	frame component.Frame,
) error {
	if err := component.ValidatePayload(frame); err != nil {
		return err
	}
	if frame.Type == component.TypeSessionRebind {
		return a.handleSessionRebind(ctx, view, frame)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := a.store.Read()
		if err != nil {
			return err
		}
		if snapshot.Catalog.Host.Generation != a.generation || view.Generation != a.generation {
			return replayError("component binding belongs to another generation")
		}
		binding, ok := snapshot.Catalog.ComponentBindings[view.BindingID]
		if !ok || binding.AttachmentID != view.AttachmentID || binding.Generation != a.generation ||
			binding.ProcessIdentity != view.ProcessIdentity || binding.BootstrapRevision != view.BootstrapRevision ||
			(binding.State != daemon.BindingBinding && binding.State != daemon.BindingReady) {
			return replayError("component binding is unavailable or fenced")
		}
		attachment, ok := snapshot.Catalog.Attachments[binding.AttachmentID]
		if !ok || attachment.Product != view.ProductID {
			return unauthorizedError("component attachment no longer matches binding")
		}
		if frame.Type == component.TypeHeartbeat {
			var heartbeat component.Heartbeat
			if err := frame.PayloadInto(&heartbeat); err != nil {
				return err
			}
			if heartbeat.BindingID != view.BindingID || heartbeat.LastReceivedSeq > view.LastOutboundSeq {
				return unauthorizedError("heartbeat conflicts with authenticated binding")
			}
		}

		catalog := snapshot.Catalog
		if frame.Type == component.TypeSessionAnnounce {
			deleted, err := prepareSessionAnnouncement(&catalog, binding, frame)
			if err != nil {
				return err
			}
			if deleted {
				if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
					if errors.Is(err, statestore.ErrConflict) {
						continue
					}
					return err
				}
				if a.afterClosedSessionDelete != nil {
					if err := a.afterClosedSessionDelete(); err != nil {
						return err
					}
				}
				continue
			}
		} else if sessionLevelFrame(frame.Type) {
			if err := requireCurrentSession(catalog, binding, frame); err != nil {
				return err
			}
		}

		if frame.Type == component.TypeHeartbeat {
			// A heartbeat is the first authenticated proof that Ready arrived. It
			// adopts the binding without requiring product/session admission.
		} else if a.admitter != nil {
			if err := a.admitter.AdmitComponentFrame(ctx, &catalog, view, frame); err != nil {
				return err
			}
		} else if frame.Type != component.TypeSessionAnnounce {
			return &component.ProtocolError{Category: component.CategoryInternal, Detail: "component frame durable handler is unavailable"}
		}

		binding = catalog.ComponentBindings[view.BindingID]
		if binding.State == daemon.BindingBinding {
			binding.State = daemon.BindingReady
		}
		if view.LastInboundSeq > binding.LastInboundSeq {
			binding.LastInboundSeq = view.LastInboundSeq
		}
		if view.LastOutboundSeq > binding.LastOutboundSeq {
			binding.LastOutboundSeq = view.LastOutboundSeq
		}
		catalog.ComponentBindings[binding.BindingID] = binding
		if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
			if errors.Is(err, statestore.ErrConflict) {
				continue
			}
			return err
		}
		if a.afterAdoptionCommit != nil {
			if err := a.afterAdoptionCommit(); err != nil {
				return err
			}
		}
		return a.collectClosedBindings(ctx)
	}
}

// handleSessionRebind preserves ComponentSession.NativeSessionID immutability
// with three convergent catalog commits: close the old row, remove it while
// changing the attachment relationship after fresh product re-attestation,
// then announce the new immutable row. Every retry derives its next step only
// from durable state.
func (a *Authority) handleSessionRebind(
	ctx context.Context,
	view component.BindingView,
	frame component.Frame,
) error {
	if a.rebinder == nil {
		return &component.ProtocolError{Category: component.CategoryInternal, Detail: "component session rebind verifier is unavailable"}
	}
	var rebind component.SessionRebind
	if err := frame.PayloadInto(&rebind); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := a.store.Read()
		if err != nil {
			return err
		}
		binding, attachment, err := a.currentFrameAuthority(snapshot, view)
		if err != nil {
			return err
		}
		if rebind.BindingID != binding.BindingID || attachment.State != "attached" {
			return unauthorizedError("session rebind attachment or binding is unavailable")
		}
		catalog := snapshot.Catalog
		session, sessionExists := catalog.ComponentSessions[binding.AttachmentID]

		switch {
		case sessionExists && session.State != daemon.ComponentSessionClosed &&
			session.BindingID == binding.BindingID && session.NativeSessionID == rebind.NewNativeSessionID &&
			attachment.NativeSessionID == rebind.NewNativeSessionID && session.LastEventSeq == rebind.ProductEventSeq:
			return a.collectClosedBindings(ctx)

		case sessionExists && session.State != daemon.ComponentSessionClosed:
			if session.BindingID != binding.BindingID || session.NativeSessionID != rebind.OldNativeSessionID ||
				attachment.NativeSessionID != rebind.OldNativeSessionID || rebind.ProductEventSeq <= session.LastEventSeq {
				return replayError("session rebind conflicts with current native relationship")
			}
			session.State = daemon.ComponentSessionClosed
			session.LastEventSeq = rebind.ProductEventSeq
			catalog.ComponentSessions[binding.AttachmentID] = session
			if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
				if errors.Is(err, statestore.ErrConflict) {
					continue
				}
				return err
			}
			if a.afterRebindClose != nil {
				if err := a.afterRebindClose(); err != nil {
					return err
				}
			}
			continue

		case sessionExists && session.State == daemon.ComponentSessionClosed:
			if session.NativeSessionID != rebind.OldNativeSessionID || session.LastEventSeq > rebind.ProductEventSeq {
				return replayError("closed session does not match rebind intent")
			}
			if session.LastEventSeq < rebind.ProductEventSeq {
				session.LastEventSeq = rebind.ProductEventSeq
				catalog.ComponentSessions[binding.AttachmentID] = session
				if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
					if errors.Is(err, statestore.ErrConflict) {
						continue
					}
					return err
				}
				if a.afterRebindClose != nil {
					if err := a.afterRebindClose(); err != nil {
						return err
					}
				}
				continue
			}
			if attachment.NativeSessionID == rebind.OldNativeSessionID {
				fresh, err := a.rebinder.ReattestSessionRebind(
					ctx, attachment, rebind.OldNativeSessionID, rebind.NewNativeSessionID, rebind.Evidence,
				)
				if err != nil || fresh.Process != binding.ProcessIdentity {
					return unauthorizedError("session rebind live evidence was refused")
				}
				attachment.NativeSessionID = rebind.NewNativeSessionID
				attachment.Evidence = fresh
				attachment.ComponentRevision++
				catalog.Host.AttachmentRevision++
				attachment.CatalogRevision = catalog.Host.AttachmentRevision
				attachment.DaemonGeneration = a.generation
				catalog.Attachments[attachment.ID] = attachment
			} else if attachment.NativeSessionID != rebind.NewNativeSessionID {
				return replayError("attachment native relationship conflicts with rebind intent")
			}
			delete(catalog.ComponentSessions, binding.AttachmentID)
			if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
				if errors.Is(err, statestore.ErrConflict) {
					continue
				}
				return err
			}
			if a.afterRebindAttachment != nil {
				if err := a.afterRebindAttachment(); err != nil {
					return err
				}
			}
			continue

		case !sessionExists && attachment.NativeSessionID == rebind.NewNativeSessionID:
			catalog.ComponentSessions[binding.AttachmentID] = daemon.ComponentSession{
				Schema: daemon.ComponentSessionRecordSchema, AttachmentID: binding.AttachmentID,
				BindingID: binding.BindingID, NativeSessionID: rebind.NewNativeSessionID,
				State: daemon.ComponentSessionAnnounced, LastEventSeq: rebind.ProductEventSeq,
			}
			if a.admitter != nil {
				if err := a.admitter.AdmitComponentFrame(ctx, &catalog, view, frame); err != nil {
					return err
				}
			}
			binding = catalog.ComponentBindings[binding.BindingID]
			if binding.State == daemon.BindingBinding {
				binding.State = daemon.BindingReady
			}
			if view.LastInboundSeq > binding.LastInboundSeq {
				binding.LastInboundSeq = view.LastInboundSeq
			}
			if view.LastOutboundSeq > binding.LastOutboundSeq {
				binding.LastOutboundSeq = view.LastOutboundSeq
			}
			catalog.ComponentBindings[binding.BindingID] = binding
			if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
				if errors.Is(err, statestore.ErrConflict) {
					continue
				}
				return err
			}
			if a.afterAdoptionCommit != nil {
				if err := a.afterAdoptionCommit(); err != nil {
					return err
				}
			}
			return a.collectClosedBindings(ctx)

		case !sessionExists && attachment.NativeSessionID == rebind.OldNativeSessionID:
			return unauthorizedError("component session must be announced before rebind")

		default:
			return replayError("session rebind durable state is conflicting")
		}
	}
}

func (a *Authority) currentFrameAuthority(
	snapshot daemon.StateSnapshot,
	view component.BindingView,
) (daemon.ComponentBinding, daemon.ManagedAttachment, error) {
	if snapshot.Catalog.Host.Generation != a.generation || view.Generation != a.generation {
		return daemon.ComponentBinding{}, daemon.ManagedAttachment{}, replayError("component binding belongs to another generation")
	}
	binding, ok := snapshot.Catalog.ComponentBindings[view.BindingID]
	if !ok || binding.AttachmentID != view.AttachmentID || binding.Generation != a.generation ||
		binding.ProcessIdentity != view.ProcessIdentity || binding.BootstrapRevision != view.BootstrapRevision ||
		(binding.State != daemon.BindingBinding && binding.State != daemon.BindingReady) {
		return daemon.ComponentBinding{}, daemon.ManagedAttachment{}, replayError("component binding is unavailable or fenced")
	}
	attachment, ok := snapshot.Catalog.Attachments[binding.AttachmentID]
	if !ok || attachment.Product != view.ProductID {
		return daemon.ComponentBinding{}, daemon.ManagedAttachment{}, unauthorizedError("component attachment no longer matches binding")
	}
	return binding, attachment, nil
}

func (a *Authority) resolve(
	ctx context.Context,
	attachmentID string,
	peer component.PeerEvidence,
) (daemon.StateSnapshot, daemon.ManagedAttachment, Resolution, error) {
	snapshot, err := a.store.Read()
	if err != nil {
		return daemon.StateSnapshot{}, daemon.ManagedAttachment{}, Resolution{}, err
	}
	if snapshot.Catalog.Host.Generation != a.generation {
		return daemon.StateSnapshot{}, daemon.ManagedAttachment{}, Resolution{}, replayError("component authority generation is stale")
	}
	attachment, ok := snapshot.Catalog.Attachments[attachmentID]
	if !ok {
		return daemon.StateSnapshot{}, daemon.ManagedAttachment{}, Resolution{}, unauthorizedError("managed attachment is unavailable")
	}
	resolution, err := a.resolver.ResolveComponent(ctx, attachment, peer)
	if err != nil {
		return daemon.StateSnapshot{}, daemon.ManagedAttachment{}, Resolution{}, unauthorizedError("component evidence could not be resolved")
	}
	if err := validateResolution(peer, attachment, resolution); err != nil {
		return daemon.StateSnapshot{}, daemon.ManagedAttachment{}, Resolution{}, err
	}
	return snapshot, attachment, resolution, nil
}

func (a *Authority) newBinding(
	attachmentID string,
	bootstrapRevision uint64,
	peer component.PeerEvidence,
) (daemon.ComponentBinding, error) {
	id, err := a.newID()
	if err != nil {
		return daemon.ComponentBinding{}, fmt.Errorf("generate component binding id: %w", err)
	}
	if !validDurableID(id) {
		return daemon.ComponentBinding{}, errors.New("generated component binding id is invalid")
	}
	return daemon.ComponentBinding{
		Schema: daemon.ComponentBindingRecordSchema, BindingID: id, AttachmentID: attachmentID,
		ProcessIdentity: peer.Process, BootstrapRevision: bootstrapRevision,
		Generation: a.generation, State: daemon.BindingBinding,
	}, nil
}

func validateBootstrapClaim(
	claim component.BootstrapClaim,
	peer component.PeerEvidence,
	attachment daemon.ManagedAttachment,
	resolution Resolution,
) error {
	if attachment.State != "prepared" {
		return unauthorizedError("component attachment is not prepared")
	}
	if claim.AttachmentID != attachment.ID || claim.ProductID != attachment.Product {
		return unauthorizedError("bootstrap attachment or product conflicts")
	}
	if resolution.BootstrapRevision != attachment.CatalogRevision {
		return unauthorizedError("bootstrap revision conflicts")
	}
	if !capabilityMatches(attachment.CapabilityHash, claim.BootstrapValue) {
		return unauthorizedError("bootstrap capability was refused")
	}
	if err := corroborateClaimedProcess("bootstrap", claim.ProcessStart, claim.StrongStart, peer.Process.Start, peer.Process.StrongStart); err != nil {
		return err
	}
	if attachment.ComponentProtocol != component.ContractRevision {
		return unauthorizedError("attachment component protocol conflicts")
	}
	if claim.ComponentVersion != component.ContractRevision {
		return unauthorizedError("component contract revision conflicts")
	}
	return nil
}

func validateReconnectClaim(
	claim component.ReconnectClaim,
	peer component.PeerEvidence,
	attachment daemon.ManagedAttachment,
	resolution Resolution,
	prior daemon.ComponentBinding,
) error {
	if attachment.State != "prepared" && attachment.State != "selecting" && attachment.State != "attached" {
		return unauthorizedError("component attachment is not reconnectable")
	}
	if attachment.ComponentProtocol != component.ContractRevision {
		return unauthorizedError("attachment component protocol conflicts")
	}
	if claim.PriorGeneration != prior.Generation || resolution.BootstrapRevision != prior.BootstrapRevision {
		return replayError("reconnect predecessor generation or anchor conflicts")
	}
	if err := corroborateClaimedProcess("reconnect", claim.ProcessStart, claim.StrongStart, peer.Process.Start, peer.Process.StrongStart); err != nil {
		return err
	}
	if prior.ProcessIdentity != peer.Process {
		return &component.ProtocolError{Category: component.CategoryStaleProcess, Detail: "reconnect process identity changed"}
	}
	if prior.State != daemon.BindingReady && prior.State != daemon.BindingRetiring && prior.State != daemon.BindingClosed {
		return replayError("reconnect predecessor was not adopted")
	}
	if prior.State != daemon.BindingReady && prior.LastInboundSeq == 0 {
		return replayError("reconnect predecessor was never adopted")
	}
	return nil
}

func corroborateClaimedProcess(operation, processStart, strongStart, liveStart, liveStrongStart string) error {
	if processStart == "" && strongStart == "" {
		return nil
	}
	if processStart == "" || strongStart == "" {
		return &component.ProtocolError{
			Category: component.CategoryInvalidFrame,
			Detail:   operation + " process corroboration must be omitted or supplied as a pair",
		}
	}
	if processStart != liveStart || strongStart != liveStrongStart {
		return &component.ProtocolError{Category: component.CategoryStaleProcess, Detail: operation + " process identity changed"}
	}
	return nil
}

func validateResolution(peer component.PeerEvidence, attachment daemon.ManagedAttachment, resolution Resolution) error {
	if !peer.Peer.Valid() || peer.Peer.PID != peer.Process.PID || peer.Process.PID <= 1 ||
		peer.Process.Start == "" || peer.Process.StrongStart == "" {
		return &component.ProtocolError{Category: component.CategoryStaleProcess, Detail: "kernel peer process evidence is invalid"}
	}
	if resolution.BootstrapRevision == 0 {
		return unauthorizedError("resolved bootstrap revision is incomplete")
	}
	if resolution.LiveEvidence.Process != peer.Process {
		return &component.ProtocolError{Category: component.CategoryStaleProcess, Detail: "live component process differs from kernel peer"}
	}
	durable := attachment.ExpectedEvidence
	if attachment.State == "attached" {
		durable = attachment.Evidence
	}
	if durable.Process == (resolution.LiveEvidence.Process) && reflect.DeepEqual(durable, resolution.LiveEvidence) {
		return nil
	}
	return unauthorizedError("live component evidence differs from durable attachment")
}

func bootstrapRows(
	catalog daemon.Catalog,
	attachmentID string,
	revision uint64,
	generation uint64,
) (*daemon.ComponentBinding, *daemon.ComponentBinding, error) {
	var current, predecessor *daemon.ComponentBinding
	for _, candidate := range catalog.ComponentBindings {
		if candidate.AttachmentID != attachmentID {
			continue
		}
		row := candidate
		if candidate.Generation == generation && (candidate.State == daemon.BindingBinding || candidate.State == daemon.BindingReady) {
			if current != nil {
				return nil, nil, replayError("attachment has duplicate live component bindings")
			}
			if candidate.BootstrapRevision != revision {
				return nil, nil, replayError("attachment has a conflicting current component binding")
			}
			current = &row
			continue
		}
		if candidate.BootstrapRevision != revision || candidate.State != daemon.BindingRetiring && candidate.State != daemon.BindingClosed {
			continue
		}
		if predecessor == nil || candidate.Generation > predecessor.Generation {
			predecessor = &row
		}
	}
	return current, predecessor, nil
}

func currentSuccessor(
	catalog daemon.Catalog,
	prior daemon.ComponentBinding,
	generation uint64,
) (*daemon.ComponentBinding, error) {
	var current *daemon.ComponentBinding
	for _, candidate := range catalog.ComponentBindings {
		if candidate.BindingID == prior.BindingID || candidate.AttachmentID != prior.AttachmentID ||
			candidate.Generation != generation ||
			(candidate.State != daemon.BindingBinding && candidate.State != daemon.BindingReady) {
			continue
		}
		if candidate.BootstrapRevision != prior.BootstrapRevision {
			return nil, replayError("attachment has a conflicting current component binding")
		}
		if current != nil {
			return nil, replayError("attachment has duplicate component successors")
		}
		row := candidate
		current = &row
	}
	return current, nil
}

func prepareSessionAnnouncement(
	catalog *daemon.Catalog,
	binding daemon.ComponentBinding,
	frame component.Frame,
) (bool, error) {
	var announce component.SessionAnnounce
	if err := frame.PayloadInto(&announce); err != nil {
		return false, err
	}
	if announce.BindingID != binding.BindingID {
		return false, unauthorizedError("session announce names another binding")
	}
	attachment := catalog.Attachments[binding.AttachmentID]
	if attachment.NativeSessionID != "" && attachment.NativeSessionID != announce.NativeSessionID {
		return false, unauthorizedError("session announce conflicts with durable native session")
	}
	if attachment.Cwd != "" && attachment.Cwd != announce.Cwd {
		return false, unauthorizedError("session announce conflicts with durable cwd")
	}
	if existing, ok := catalog.ComponentSessions[binding.AttachmentID]; ok {
		if existing.State == daemon.ComponentSessionClosed {
			if existing.NativeSessionID != announce.NativeSessionID {
				return false, replayError("closed component session conflicts with re-announcement")
			}
			delete(catalog.ComponentSessions, binding.AttachmentID)
			return true, nil
		}
		if existing.BindingID != binding.BindingID || existing.NativeSessionID != announce.NativeSessionID ||
			announce.ProductEventSeq < existing.LastEventSeq {
			return false, replayError("session announce conflicts with current component session")
		}
		existing.LastEventSeq = announce.ProductEventSeq
		catalog.ComponentSessions[binding.AttachmentID] = existing
		return false, nil
	}
	catalog.ComponentSessions[binding.AttachmentID] = daemon.ComponentSession{
		Schema: daemon.ComponentSessionRecordSchema, AttachmentID: binding.AttachmentID,
		BindingID: binding.BindingID, NativeSessionID: announce.NativeSessionID,
		State: daemon.ComponentSessionAnnounced, LastEventSeq: announce.ProductEventSeq,
	}
	return false, nil
}

func requireCurrentSession(catalog daemon.Catalog, binding daemon.ComponentBinding, frame component.Frame) error {
	session, ok := catalog.ComponentSessions[binding.AttachmentID]
	if !ok || session.State == daemon.ComponentSessionClosed || session.BindingID != binding.BindingID {
		return unauthorizedError("component session must be announced first")
	}
	nativeID := frameNativeSessionID(frame)
	if nativeID != "" && nativeID != session.NativeSessionID {
		return unauthorizedError("component frame names another native session")
	}
	return nil
}

func frameNativeSessionID(frame component.Frame) string {
	switch frame.Type {
	case component.TypeSessionRebind:
		var value component.SessionRebind
		_ = frame.PayloadInto(&value)
		return value.OldNativeSessionID
	case component.TypeSessionRename:
		var value component.SessionRename
		_ = frame.PayloadInto(&value)
		return value.NativeSessionID
	case component.TypeSessionState:
		var value component.SessionState
		_ = frame.PayloadInto(&value)
		return value.NativeSessionID
	case component.TypeSessionClose:
		var value component.SessionClose
		_ = frame.PayloadInto(&value)
		return value.NativeSessionID
	case component.TypeDeliveryAccept:
		var value component.DeliveryAccept
		_ = frame.PayloadInto(&value)
		return value.NativeSessionID
	case component.TypeTurnEvent:
		var value component.TurnEvent
		_ = frame.PayloadInto(&value)
		return value.NativeSessionID
	default:
		return ""
	}
}

func sessionLevelFrame(frameType component.FrameType) bool {
	switch frameType {
	case component.TypeSessionRebind, component.TypeSessionRename, component.TypeSessionState,
		component.TypeSessionClose, component.TypeDeliveryAccept, component.TypeDeliveryReject, component.TypeTurnEvent,
		component.TypeToolCall, component.TypeToolCancel:
		return true
	default:
		return false
	}
}

func closeSessionForBinding(catalog *daemon.Catalog, bindingID string) {
	for id, session := range catalog.ComponentSessions {
		if session.BindingID == bindingID && session.State != daemon.ComponentSessionClosed {
			session.State = daemon.ComponentSessionClosed
			catalog.ComponentSessions[id] = session
		}
	}
}

func (a *Authority) collectClosedBindings(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := a.store.Read()
		if err != nil {
			return err
		}
		keep := closedPredecessorsToKeep(snapshot.Catalog)
		catalog := snapshot.Catalog
		changed := false
		for id, binding := range catalog.ComponentBindings {
			if binding.State == daemon.BindingClosed && !keep[id] {
				delete(catalog.ComponentBindings, id)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if _, err := a.store.Commit(snapshot.Revision, catalog); err != nil {
			if errors.Is(err, statestore.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
}

func closedPredecessorsToKeep(catalog daemon.Catalog) map[string]bool {
	keep := map[string]bool{}
	type anchor struct {
		attachment string
		revision   uint64
	}
	currents := map[anchor]bool{}
	for _, binding := range catalog.ComponentBindings {
		if binding.State == daemon.BindingBinding {
			currents[anchor{binding.AttachmentID, binding.BootstrapRevision}] = true
		}
	}
	closed := map[anchor][]daemon.ComponentBinding{}
	for _, binding := range catalog.ComponentBindings {
		key := anchor{binding.AttachmentID, binding.BootstrapRevision}
		if binding.State == daemon.BindingClosed && currents[key] {
			closed[key] = append(closed[key], binding)
		}
	}
	for _, candidates := range closed {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Generation != candidates[j].Generation {
				return candidates[i].Generation > candidates[j].Generation
			}
			return candidates[i].BindingID < candidates[j].BindingID
		})
		keep[candidates[0].BindingID] = true
	}
	return keep
}

func authorization(
	binding daemon.ComponentBinding,
	attachment daemon.ManagedAttachment,
	resolution Resolution,
) component.Authorization {
	return component.Authorization{
		BindingID: binding.BindingID, AttachmentID: binding.AttachmentID, ProductID: attachment.Product,
		ProcessIdentity: binding.ProcessIdentity, Executable: resolution.LiveEvidence.Executable,
		BootstrapRevision: binding.BootstrapRevision,
	}
}

func randomBindingID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return bindingIDPrefix + hex.EncodeToString(random[:]), nil
}

func capabilityMatches(expectedHash, capability string) bool {
	expected, err := hex.DecodeString(expectedHash)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	actual := sha256.Sum256([]byte(capability))
	return subtle.ConstantTimeCompare(expected, actual[:]) == 1
}

func validDurableID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func unauthorizedError(detail string) error {
	return &component.ProtocolError{Category: component.CategoryUnauthorized, Detail: detail}
}

func replayError(detail string) error {
	return &component.ProtocolError{Category: component.CategoryReplay, Detail: detail}
}
