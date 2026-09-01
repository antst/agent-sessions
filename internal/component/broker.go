package component

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/localtransport"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const (
	ComponentSocketName     = "component.sock"
	defaultReplayWindow     = 64
	defaultMaxOutstanding   = 128
	defaultHeartbeatGrace   = 3
	defaultHandshakeTimeout = 5 * time.Second
	defaultAncestryDepth    = 32
)

// PeerEvidence is captured by the broker before authorization. Process is an
// exact live start/strong-start token for the kernel-authenticated peer PID.
type PeerEvidence struct {
	Peer    localtransport.PeerIdentity
	Process procinfo.Identity
}

// Authorization is secret-free daemon evidence returned after one-time
// bootstrap consumption or durable reconnect lookup. ProcessIdentity must
// exactly match the broker's fresh capture. AncestorIdentity and Executable
// add lineage checks when the prepared attachment requires them.
type Authorization struct {
	AttachmentID      string
	ProductID         string
	ProcessIdentity   procinfo.Identity
	AncestorIdentity  procinfo.Identity
	Executable        string
	BootstrapRevision uint64
}

// Authorizer supplies daemon-owned bootstrap consumption and durable
// attachment lookup. Implementations must validate capability hash/revision,
// expected product, attachment, lineage evidence, and reconnect relationship.
type Authorizer interface {
	Bootstrap(context.Context, BootstrapClaim, PeerEvidence) (Authorization, error)
	Reconnect(context.Context, ReconnectClaim, PeerEvidence) (Authorization, error)
}

// Handler receives authenticated component-to-daemon operations. It does not
// own framing or connection lifecycle.
type Handler interface {
	HandleComponentFrame(context.Context, BindingView, Frame) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, BindingView, Frame) error

func (f HandlerFunc) HandleComponentFrame(ctx context.Context, binding BindingView, frame Frame) error {
	return f(ctx, binding, frame)
}

// Config fixes one daemon generation's component broker limits.
type Config struct {
	Generation        uint64
	Authorizer        Authorizer
	Handler           Handler
	Limits            localtransport.Limits
	ReplayWindow      uint64
	MaxOutstanding    int
	HeartbeatInterval time.Duration
	HeartbeatGrace    int
	HandshakeTimeout  time.Duration
	MaxAncestryDepth  int
}

// Broker owns live generation-scoped component streams only. Durable
// attachment/session/delivery authority remains behind Authorizer and Handler.
type Broker struct {
	config   Config
	mu       sync.Mutex
	bindings map[string]*binding
	wg       sync.WaitGroup
}

// NewBroker validates and normalizes one generation's fixed bounds.
func NewBroker(config Config) (*Broker, error) {
	if config.Generation == 0 {
		return nil, errors.New("component broker generation must be positive")
	}
	if config.Authorizer == nil {
		return nil, errors.New("component broker authorizer is required")
	}
	if config.Limits == (localtransport.Limits{}) {
		config.Limits = localtransport.DefaultLimits()
	}
	defaultLimits := localtransport.DefaultLimits()
	if !config.Limits.Valid() || config.Limits.MaxFrameBytes > defaultLimits.MaxFrameBytes ||
		config.Limits.MaxNesting > defaultLimits.MaxNesting || config.Limits.MaxStringBytes > defaultLimits.MaxStringBytes {
		return nil, errors.New("component transport limits are invalid or widen protocol defaults")
	}
	if config.ReplayWindow == 0 {
		config.ReplayWindow = defaultReplayWindow
	}
	if config.ReplayWindow > 4096 {
		return nil, errors.New("component replay window exceeds fixed maximum")
	}
	if config.MaxOutstanding == 0 {
		config.MaxOutstanding = defaultMaxOutstanding
	}
	if config.MaxOutstanding < 1 || config.MaxOutstanding > 4096 {
		return nil, errors.New("component outstanding-operation bound is invalid")
	}
	if config.HeartbeatInterval < time.Millisecond {
		return nil, errors.New("component heartbeat interval must be positive")
	}
	if config.HeartbeatGrace == 0 {
		config.HeartbeatGrace = defaultHeartbeatGrace
	}
	if config.HeartbeatGrace < 1 || config.HeartbeatGrace > 10 {
		return nil, errors.New("component heartbeat grace is invalid")
	}
	if config.HandshakeTimeout == 0 {
		config.HandshakeTimeout = defaultHandshakeTimeout
	}
	if config.HandshakeTimeout < time.Millisecond || config.HandshakeTimeout > time.Minute {
		return nil, errors.New("component handshake timeout is invalid")
	}
	if config.MaxAncestryDepth == 0 {
		config.MaxAncestryDepth = defaultAncestryDepth
	}
	if config.MaxAncestryDepth < 0 || config.MaxAncestryDepth > 256 {
		return nil, errors.New("component ancestry depth is invalid")
	}
	return &Broker{config: config, bindings: make(map[string]*binding)}, nil
}

// Serve listens only on the dedicated component.sock path and serves until
// context cancellation. The one-shot daemon.sock is never accepted here.
func (b *Broker) Serve(ctx context.Context, path string) error {
	if filepath.Base(path) != ComponentSocketName {
		return fmt.Errorf("component broker socket must be named %s", ComponentSocketName)
	}
	listener, err := localtransport.Listen(path, b.config.Limits)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		b.closeBindings()
	}()
	for {
		connection, peer, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				b.wg.Wait()
				return nil
			}
			continue
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handleConnection(ctx, connection, peer)
		}()
	}
}

// Bindings returns immutable snapshots for diagnostics/coordinator routing.
func (b *Broker) Bindings() []BindingView {
	b.mu.Lock()
	bindings := make([]*binding, 0, len(b.bindings))
	for _, live := range b.bindings {
		bindings = append(bindings, live)
	}
	b.mu.Unlock()
	result := make([]BindingView, 0, len(bindings))
	for _, live := range bindings {
		result = append(result, live.snapshot())
	}
	return result
}

// Send writes one daemon-to-component frame to an exact live binding.
func (b *Broker) Send(bindingID string, frameType FrameType, operationID string, payload any) error {
	if !daemonToComponent(frameType) {
		return fmt.Errorf("frame type %s is not daemon-to-component", frameType)
	}
	b.mu.Lock()
	live := b.bindings[bindingID]
	b.mu.Unlock()
	if live == nil {
		return errors.New("component binding is unavailable")
	}
	return live.send(frameType, operationID, payload)
}

// RetireGeneration fences all live bindings with a best-effort retirement
// frame and closes them. Durable operations remain owned by the daemon.
func (b *Broker) RetireGeneration() {
	b.mu.Lock()
	bindings := make([]*binding, 0, len(b.bindings))
	for _, live := range b.bindings {
		bindings = append(bindings, live)
	}
	b.mu.Unlock()
	for _, live := range bindings {
		view := live.snapshot()
		_ = live.send(TypeGenerationRetire, view.BindingID, GenerationRetire{BindingID: view.BindingID, Generation: view.Generation})
		_ = live.close()
	}
}

func (b *Broker) closeBindings() {
	b.mu.Lock()
	bindings := make([]*binding, 0, len(b.bindings))
	for _, live := range b.bindings {
		bindings = append(bindings, live)
	}
	b.mu.Unlock()
	for _, live := range bindings {
		_ = live.close()
	}
}

func (b *Broker) handleConnection(ctx context.Context, connection *localtransport.Conn, peer localtransport.PeerIdentity) {
	defer connection.Close()
	if peer.UID != os.Geteuid() {
		b.writeHandshakeReject(connection, "unauthorized", CategoryUnauthorized, "kernel peer uid does not match daemon uid")
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(b.config.HandshakeTimeout))
	body, err := connection.ReadFrame()
	if err != nil {
		return
	}
	first, err := DecodeFrame(body)
	if err != nil {
		b.writeProtocolReject(connection, "invalid-frame", 1, err)
		return
	}
	if err := ValidatePayload(first); err != nil {
		b.writeProtocolReject(connection, first.ID, 1, err)
		return
	}
	if first.Type != TypeBootstrap && first.Type != TypeReconnect {
		b.writeHandshakeReject(connection, first.ID, CategoryProtocol, "first component frame must bootstrap or reconnect")
		return
	}
	evidence, err := captureEvidence(peer)
	if err != nil {
		b.writeHandshakeReject(connection, first.ID, CategoryStaleProcess, err.Error())
		return
	}
	authorization, secret, err := b.authorize(ctx, first, evidence)
	if err != nil {
		b.writeHandshakeReject(connection, first.ID, categoryFromError(err, CategoryUnauthorized), Redact(err.Error(), secret))
		return
	}
	if err := b.validateAuthorization(first, evidence, authorization); err != nil {
		b.writeHandshakeReject(connection, first.ID, CategoryStaleProcess, err.Error())
		return
	}
	if first.Type == TypeBootstrap {
		wipeBytes(first.Payload)
		wipeBytes(body)
		first.Payload = nil
		body = nil
		secret = ""
	}
	bindingID, err := newBindingID()
	if err != nil {
		b.writeHandshakeReject(connection, first.ID, CategoryInternal, "cannot allocate binding id")
		return
	}
	live := &binding{
		view: BindingView{
			BindingID: bindingID, AttachmentID: authorization.AttachmentID, ProductID: authorization.ProductID,
			ProcessIdentity: evidence.Process, PeerIdentity: evidence.Peer, Generation: b.config.Generation,
			BootstrapRevision: authorization.BootstrapRevision, LastInboundSeq: first.Seq,
		},
		connection: connection, replayWindow: b.config.ReplayWindow, maxOutstanding: b.config.MaxOutstanding,
		inboundDigests:   make(map[uint64][sha256.Size]byte),
		pendingToolCalls: make(map[string][sha256.Size]byte), pendingDeliveries: make(map[string]struct{}),
		completedDelivery: make(map[string][sha256.Size]byte),
		lastHeartbeat:     time.Now(), heartbeatInterval: b.config.HeartbeatInterval, heartbeatGrace: b.config.HeartbeatGrace,
	}
	b.mu.Lock()
	b.bindings[bindingID] = live
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.bindings[bindingID] == live {
			delete(b.bindings, bindingID)
		}
		b.mu.Unlock()
		_ = live.close()
	}()
	ready := Ready{
		BindingID: bindingID, AttachmentID: authorization.AttachmentID, DaemonGeneration: b.config.Generation,
		ProtocolVersion: ProtocolVersion, MaxFrameBytes: b.config.Limits.MaxFrameBytes,
		HeartbeatIntervalMS: b.config.HeartbeatInterval.Milliseconds(),
	}
	if err := live.send(TypeReady, first.ID, ready); err != nil {
		return
	}
	b.readBinding(ctx, live)
}

func captureEvidence(peer localtransport.PeerIdentity) (PeerEvidence, error) {
	if !peer.Valid() {
		return PeerEvidence{}, errors.New("kernel peer identity is invalid")
	}
	identity, err := procinfo.CaptureIdentity(peer.PID)
	if err != nil {
		return PeerEvidence{}, err
	}
	return PeerEvidence{Peer: peer, Process: identity}, nil
}

func (b *Broker) authorize(ctx context.Context, first Frame, evidence PeerEvidence) (authorization Authorization, bootstrapSecret string, err error) {
	if first.Type == TypeBootstrap {
		var claim BootstrapClaim
		if err := first.PayloadInto(&claim); err != nil {
			return Authorization{}, "", err
		}
		bootstrapSecret = claim.BootstrapValue
		if claim.ProcessStart != evidence.Process.Start || claim.StrongStart != evidence.Process.StrongStart {
			return Authorization{}, bootstrapSecret, &ProtocolError{Category: CategoryStaleProcess, Detail: "bootstrap process start does not match kernel peer"}
		}
		authorization, err = b.config.Authorizer.Bootstrap(ctx, claim, evidence)
		if err != nil {
			return authorization, bootstrapSecret, err
		}
		return authorization, "", nil
	}
	var claim ReconnectClaim
	if err := first.PayloadInto(&claim); err != nil {
		return Authorization{}, "", err
	}
	if claim.PriorGeneration >= b.config.Generation {
		return Authorization{}, "", &ProtocolError{Category: CategoryProtocol, Detail: "reconnect generation is not retired"}
	}
	if claim.ProcessStart != evidence.Process.Start || claim.StrongStart != evidence.Process.StrongStart {
		return Authorization{}, "", &ProtocolError{Category: CategoryStaleProcess, Detail: "reconnect process start does not match kernel peer"}
	}
	authorization, err = b.config.Authorizer.Reconnect(ctx, claim, evidence)
	return authorization, "", err
}

func (b *Broker) validateAuthorization(first Frame, evidence PeerEvidence, authorization Authorization) error {
	if !validIdentifier(authorization.AttachmentID) || !validProductID(authorization.ProductID) || authorization.BootstrapRevision == 0 {
		return errors.New("authorizer returned incomplete attachment evidence")
	}
	if authorization.ProcessIdentity != evidence.Process {
		return errors.New("authorized process identity no longer matches kernel peer")
	}
	if first.Type == TypeBootstrap {
		var claim BootstrapClaim
		_ = first.PayloadInto(&claim)
		if authorization.AttachmentID != claim.AttachmentID || authorization.ProductID != claim.ProductID {
			return errors.New("authorized attachment or product does not match bootstrap")
		}
	} else {
		var claim ReconnectClaim
		_ = first.PayloadInto(&claim)
		if authorization.AttachmentID != claim.AttachmentID {
			return errors.New("authorized attachment does not match reconnect")
		}
	}
	if authorization.AncestorIdentity.PID > 1 {
		matches, err := procinfo.DescendsFrom(evidence.Process, authorization.AncestorIdentity, b.config.MaxAncestryDepth)
		if err != nil {
			return fmt.Errorf("corroborate component ancestry: %w", err)
		}
		if !matches {
			return errors.New("component process does not descend from prepared lineage")
		}
	}
	if authorization.Executable != "" {
		arguments, err := procinfo.Args(evidence.Process.PID)
		if err != nil || len(arguments) == 0 || !sameExecutable(arguments[0], authorization.Executable) {
			return errors.New("component executable does not match prepared attachment")
		}
	}
	return nil
}

func sameExecutable(observed, expected string) bool {
	observed = filepath.Clean(observed)
	expected = filepath.Clean(expected)
	if observed == expected {
		return true
	}
	observedReal, observedErr := filepath.EvalSymlinks(observed)
	expectedReal, expectedErr := filepath.EvalSymlinks(expected)
	return observedErr == nil && expectedErr == nil && observedReal == expectedReal
}

func (b *Broker) readBinding(ctx context.Context, live *binding) {
	for ctx.Err() == nil {
		_ = live.connection.SetReadDeadline(live.heartbeatDeadline())
		body, err := live.connection.ReadFrame()
		if err != nil {
			return
		}
		frame, err := DecodeFrame(body)
		if err != nil {
			b.rejectLive(live, "invalid-frame", err)
			return
		}
		if err := ValidatePayload(frame); err != nil {
			b.rejectLive(live, frame.ID, err)
			return
		}
		if !componentToDaemon(frame.Type) {
			b.rejectLive(live, frame.ID, &ProtocolError{Category: CategoryProtocol, Detail: "frame direction is invalid"})
			return
		}
		duplicate, err := live.acceptInbound(frame, body)
		if err != nil {
			b.rejectLive(live, frame.ID, err)
			return
		}
		if duplicate {
			if frame.Type == TypeHeartbeat {
				if err := validateBindingReference(live.snapshot(), frame); err != nil {
					b.rejectLive(live, frame.ID, err)
					return
				}
				live.markHeartbeat()
				if err := live.send(TypeHeartbeatAck, frame.ID, Heartbeat{BindingID: live.snapshot().BindingID, LastReceivedSeq: frame.Seq}); err != nil {
					return
				}
			}
			continue
		}
		if err := validateBindingReference(live.snapshot(), frame); err != nil {
			b.rejectLive(live, frame.ID, err)
			return
		}
		frame = redactInbound(frame)
		if frame.Type == TypeHeartbeat {
			live.markHeartbeat()
			if err := live.send(TypeHeartbeatAck, frame.ID, Heartbeat{BindingID: live.snapshot().BindingID, LastReceivedSeq: frame.Seq}); err != nil {
				return
			}
			continue
		}
		operationReplay, err := live.trackOperation(frame)
		if err != nil {
			b.rejectLive(live, frame.ID, err)
			continue
		}
		if operationReplay {
			continue
		}
		if b.config.Handler != nil {
			if err := b.config.Handler.HandleComponentFrame(ctx, live.snapshot(), frame); err != nil {
				b.rejectLive(live, frame.ID, err)
			}
		}
	}
}

func redactInbound(frame Frame) Frame {
	if frame.Type != TypeDeliveryReject {
		return frame
	}
	var value DeliveryReject
	if err := frame.PayloadInto(&value); err != nil {
		return frame
	}
	value.Detail = Redact(value.Detail)
	redacted, err := NewFrame(frame.Type, frame.ID, frame.Seq, value)
	if err != nil {
		return frame
	}
	return redacted
}

func validateBindingReference(view BindingView, frame Frame) error {
	switch frame.Type {
	case TypeSessionAnnounce:
		var value SessionAnnounce
		_ = frame.PayloadInto(&value)
		if value.BindingID != view.BindingID {
			return &ProtocolError{Category: CategoryUnauthorized, Detail: "session announce names another binding"}
		}
	case TypeSessionRebind:
		var value SessionRebind
		_ = frame.PayloadInto(&value)
		if value.BindingID != view.BindingID {
			return &ProtocolError{Category: CategoryUnauthorized, Detail: "session rebind names another binding"}
		}
	case TypeHeartbeat:
		var value Heartbeat
		_ = frame.PayloadInto(&value)
		if value.BindingID != view.BindingID || value.LastReceivedSeq > view.LastOutboundSeq {
			return &ProtocolError{Category: CategoryProtocol, Detail: "heartbeat binding or receive sequence is invalid"}
		}
	}
	return nil
}

func componentToDaemon(frameType FrameType) bool {
	switch frameType {
	case TypeSessionAnnounce, TypeSessionRebind, TypeSessionRename, TypeSessionState, TypeSessionClose,
		TypeDeliveryAccept, TypeDeliveryReject, TypeTurnEvent, TypeToolCall, TypeToolCancel, TypeHeartbeat:
		return true
	default:
		return false
	}
}

func daemonToComponent(frameType FrameType) bool {
	switch frameType {
	case TypeSessionBound, TypeDeliveryPresent, TypeToolResult, TypeGenerationRetire, TypeHeartbeatAck, TypeReject:
		return true
	default:
		return false
	}
}

func (b *Broker) rejectLive(live *binding, operationID string, err error) {
	category := categoryFromError(err, CategoryProtocol)
	_ = live.send(TypeReject, safeOperationID(operationID), Reject{OperationID: safeOperationID(operationID), Category: category, Detail: Redact(err.Error())})
}

func (b *Broker) writeProtocolReject(connection *localtransport.Conn, operationID string, seq uint64, err error) {
	category := categoryFromError(err, CategoryProtocol)
	frame, frameErr := NewFrame(TypeReject, safeOperationID(operationID), seq, Reject{OperationID: safeOperationID(operationID), Category: category, Detail: Redact(err.Error())})
	if frameErr != nil {
		return
	}
	body, frameErr := EncodeFrame(frame)
	if frameErr == nil {
		_ = connection.WriteFrame(body)
	}
}

func (b *Broker) writeHandshakeReject(connection *localtransport.Conn, operationID string, category Category, detail string) {
	b.writeProtocolReject(connection, operationID, 1, &ProtocolError{Category: category, Detail: Redact(detail)})
}

func categoryFromError(err error, fallback Category) Category {
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) && protocolErr.Category != "" {
		return protocolErr.Category
	}
	return fallback
}

func safeOperationID(value string) string {
	value = strings.TrimSpace(value)
	if validIdentifier(value) {
		return value
	}
	return "invalid-frame"
}

func newBindingID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "binding-" + hex.EncodeToString(value[:]), nil
}

func wipeBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
