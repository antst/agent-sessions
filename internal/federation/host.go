package federation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/sessionkey"
)

const (
	maxEmbeddedLaneOutput = 16 << 20
	maxRemoteLaneRuns     = 32
	maxRemoteLaneArgs     = 256
	maxRemoteLaneArgBytes = 512 * 1024
)

// EmbeddedHostOptions connects the one user daemon to a protocol-compatible
// hub. Native product behavior remains behind the callbacks.
type EmbeddedHostOptions struct {
	Hub               string
	HostID            string
	HostName          string
	Capabilities      []string
	Generation        uint64
	Build             string
	ScanInterval      time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	Snapshot          func(context.Context) ([]Peer, error)
	Deliver           func(context.Context, Peer, Peer, AgentFrame) error
	RunLane           func(context.Context, RemoteLaneRequest) (RemoteLaneResult, error)
	Logger            *log.Logger
}

// RemoteLaneRequest is one hub-attested lane operation delivered to a daemon.
type RemoteLaneRequest struct {
	Source         Peer
	Parent         ParentContext
	TargetHostID   string
	Product        string
	Capability     string
	Arguments      []string
	Input          []byte
	IdempotencyKey string
}

// RemoteLaneResult is the bounded result returned over the lane stream.
type RemoteLaneResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type pendingLane struct {
	responses  chan Message
	failed     chan string
	cancelOnce sync.Once
}

type laneRun struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	cancelled bool
}

type deliveryOutcome struct {
	err error
}

// EmbeddedHost is the actual outbound federation state machine owned by one
// daemon. It owns reconnects, remote rosters, delivery acknowledgements and
// deduplication, and remote lane streams.
type EmbeddedHost struct {
	options EmbeddedHostOptions
	logger  *log.Logger

	mu           sync.RWMutex
	local        map[string]Peer
	remote       map[string]Peer
	remoteHosts  map[string]Host
	network      *wireConn
	localChanged chan struct{}

	deliveryMu        sync.Mutex
	pendingDeliveries map[string]chan deliveryOutcome

	laneMu       sync.Mutex
	pendingLanes map[string]*pendingLane
	laneRuns     map[string]*laneRun
}

// NewEmbeddedHost validates and constructs the daemon-owned host engine.
func NewEmbeddedHost(options EmbeddedHostOptions) (*EmbeddedHost, error) {
	if !validSimpleID(options.HostID) || len(options.HostID) > maxHostIDBytes {
		return nil, errors.New("embedded federation host id must be a simple stable identifier")
	}
	if strings.TrimSpace(options.HostName) == "" {
		options.HostName = options.HostID
	}
	if options.Snapshot == nil || options.Deliver == nil {
		return nil, errors.New("embedded federation requires snapshot and delivery callbacks")
	}
	if options.ScanInterval <= 0 {
		options.ScanInterval = time.Second
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = 5 * time.Second
	}
	if options.HeartbeatTimeout <= 0 {
		options.HeartbeatTimeout = 20 * time.Second
	}
	capabilities, err := normalizeCapabilities(options.Capabilities)
	if err != nil {
		return nil, err
	}
	options.Capabilities = capabilities
	if len(options.Capabilities) != 0 && options.RunLane == nil {
		return nil, errors.New("embedded federation lane capabilities require a lane callback")
	}
	if options.Build == "" {
		options.Build = RuntimeVersion
	}
	if len(options.HostName) > maxHostNameBytes || len(options.Build) > maxBuildBytes {
		return nil, errors.New("embedded federation host metadata exceeds protocol bounds")
	}
	return &EmbeddedHost{
		options: options,
		logger:  defaultLogger(options.Logger),
		local:   map[string]Peer{}, remote: map[string]Peer{}, remoteHosts: map[string]Host{},
		localChanged:      make(chan struct{}, 1),
		pendingDeliveries: map[string]chan deliveryOutcome{},
		pendingLanes:      map[string]*pendingLane{}, laneRuns: map[string]*laneRun{},
	}, nil
}

// Run maintains the hub connection until ctx is canceled. A network failure
// clears transient remote state and reconnects with bounded exponential delay.
func (h *EmbeddedHost) Run(ctx context.Context) error {
	if err := h.refreshLocal(ctx); err != nil {
		return err
	}
	defer func() {
		h.setNetwork(nil)
		h.clearRemote()
	}()
	if h.options.Hub == "" {
		<-ctx.Done()
		return nil
	}
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		// The daemon catalog remains authoritative while the hub is absent. Refresh
		// before every connection attempt so the first post-reconnect snapshot can
		// never resurrect peers that changed during an outage.
		if err := h.refreshLocal(ctx); err != nil {
			h.logger.Printf("local federation snapshot failed while disconnected: %v", err)
		}
		err := h.runHubSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		h.logger.Printf("hub session ended: %v", err)
		h.setNetwork(nil)
		h.clearRemote()
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
}

func (h *EmbeddedHost) runHubSession(ctx context.Context) error {
	conn, err := net.DialTimeout("tcp", h.options.Hub, 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	wire := newWireConn(conn)
	if err := wire.Send(Message{
		Type: "hello", Version: ProtocolVersion, Build: h.options.Build, Generation: h.options.Generation,
		HostID: h.options.HostID, HostName: h.options.HostName,
		Capabilities: append([]string(nil), h.options.Capabilities...),
	}); err != nil {
		return err
	}
	readErr, ready, lastHubActivity := h.startHubSessionReader(conn)
	handshakeReady, err := waitForHubHandshake(ctx, ready, readErr)
	if err != nil {
		return err
	}
	if !handshakeReady {
		return nil
	}
	h.setNetwork(wire)
	previous := h.localSnapshot()
	if err := wire.Send(Message{Type: "snapshot", Peers: previous}); err != nil {
		return err
	}
	h.logger.Printf("connected to hub %s as %s", h.options.Hub, h.options.HostID)
	return h.serveHubSession(ctx, wire, readErr, lastHubActivity, previous)
}

func (h *EmbeddedHost) startHubSessionReader(conn net.Conn) (<-chan error, <-chan error, *atomic.Int64) {
	readErr := make(chan error, 1)
	ready := make(chan error, 1)
	var first atomic.Bool
	first.Store(true)
	lastHubActivity := &atomic.Int64{}
	lastHubActivity.Store(time.Now().UnixNano())
	go func() {
		readErr <- scanMessages(conn, func(message Message) error {
			lastHubActivity.Store(time.Now().UnixNano())
			if first.CompareAndSwap(true, false) {
				if message.Type != "hello_ok" || message.Version != ProtocolVersion {
					err := errors.New("hub refused the protocol handshake")
					ready <- err
					return err
				}
				ready <- nil
				return nil
			}
			return h.handleHubMessage(message)
		})
	}()
	return readErr, ready, lastHubActivity
}

func waitForHubHandshake(ctx context.Context, ready, readErr <-chan error) (bool, error) {
	select {
	case <-ctx.Done():
		return false, nil
	case err := <-ready:
		if err != nil {
			return false, err
		}
	case err := <-readErr:
		return false, err
	}
	return true, nil
}

func (h *EmbeddedHost) serveHubSession(
	ctx context.Context,
	wire *wireConn,
	readErr <-chan error,
	lastHubActivity *atomic.Int64,
	previous []Peer,
) error {
	scanTicker := time.NewTicker(h.options.ScanInterval)
	pingTicker := time.NewTicker(h.options.HeartbeatInterval)
	defer scanTicker.Stop()
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case <-h.localChanged:
			if err := h.sendChangedSnapshot(wire, &previous); err != nil {
				return err
			}
		case <-scanTicker.C:
			if err := h.refreshLocal(ctx); err != nil {
				h.logger.Printf("local federation snapshot failed: %v", err)
			}
		case <-pingTicker.C:
			if time.Since(time.Unix(0, lastHubActivity.Load())) > h.options.HeartbeatTimeout {
				return errors.New("hub heartbeat timed out")
			}
			if err := wire.Send(Message{Type: "ping"}); err != nil {
				return err
			}
		}
	}
}

func (h *EmbeddedHost) sendChangedSnapshot(wire *wireConn, previous *[]Peer) error {
	current := h.localSnapshot()
	if reflect.DeepEqual(*previous, current) {
		return nil
	}
	if err := wire.Send(Message{Type: "snapshot", Peers: current}); err != nil {
		return err
	}
	*previous = current
	return nil
}

func (h *EmbeddedHost) refreshLocal(ctx context.Context) error {
	peers, err := h.options.Snapshot(ctx)
	if err != nil {
		return err
	}
	if len(peers) > maxSnapshotPeers {
		return fmt.Errorf("embedded federation snapshot exceeds peer count limit %d", maxSnapshotPeers)
	}
	next := make(map[string]Peer, len(peers))
	for _, peer := range peers {
		if err := validateLocalPeer(peer, h.options.HostID); err != nil {
			return fmt.Errorf("invalid embedded peer %s: %w", peer.ID, err)
		}
		if _, exists := next[peer.ID]; exists {
			return fmt.Errorf("duplicate embedded peer %s", peer.ID)
		}
		next[peer.ID] = clonePeer(peer)
	}
	h.mu.Lock()
	changed := !reflect.DeepEqual(h.local, next)
	h.local = next
	h.mu.Unlock()
	if changed {
		select {
		case h.localChanged <- struct{}{}:
		default:
		}
	}
	return nil
}

func (h *EmbeddedHost) localSnapshot() []Peer {
	h.mu.RLock()
	peers := clonePeerMap(h.local)
	h.mu.RUnlock()
	return peers
}

func (h *EmbeddedHost) setNetwork(wire *wireConn) {
	h.mu.Lock()
	h.network = wire
	h.mu.Unlock()
	if wire == nil {
		h.failPendingDeliveries("hub is disconnected")
		h.failPendingLanes("hub is disconnected")
		h.cancelAllLaneRuns()
	}
}

func (h *EmbeddedHost) clearRemote() {
	h.mu.Lock()
	h.remote = map[string]Peer{}
	h.remoteHosts = map[string]Host{}
	h.mu.Unlock()
}

//nolint:gocyclo // Each protocol frame has a closed, explicit ownership path.
func (h *EmbeddedHost) handleHubMessage(message Message) error {
	switch message.Type {
	case "pong":
		return nil
	case "roster":
		if message.Version != ProtocolVersion {
			return errors.New("hub roster protocol is incompatible")
		}
		if len(message.Hosts) > maxRosterHosts || len(message.Peers) > maxRosterPeers {
			return errors.New("hub roster exceeds protocol count bounds")
		}
		remoteHosts := make(map[string]Host, len(message.Hosts))
		for _, host := range message.Hosts {
			if err := validateRosterHost(host); err != nil {
				return fmt.Errorf("hub roster contains an invalid host: %w", err)
			}
			if host.ID != h.options.HostID {
				capabilities, err := normalizeCapabilities(host.Capabilities)
				if err != nil {
					return fmt.Errorf("hub roster host %s capabilities: %w", host.ID, err)
				}
				host.Capabilities = capabilities
				remoteHosts[host.ID] = host
			}
		}
		remote := make(map[string]Peer, len(message.Peers))
		for _, peer := range message.Peers {
			if err := validateWirePeer(peer, peer.HostID); err != nil {
				return fmt.Errorf("hub roster contains invalid peer: %w", err)
			}
			if peer.HostID != h.options.HostID {
				if _, ok := remoteHosts[peer.HostID]; !ok {
					return errors.New("hub roster peer has no connected host")
				}
				remote[peer.ID] = clonePeer(peer)
			}
		}
		h.mu.Lock()
		h.remote, h.remoteHosts = remote, remoteHosts
		h.mu.Unlock()
		return nil
	case "group_deliver", "terminal_notice_deliver":
		err := h.deliverInbound(message)
		h.sendDeliveryOutcome(message, err)
		return nil
	case "delivery_ack", "delivery_error":
		h.completePendingDelivery(message)
		return nil
	case "lane_exec":
		h.startLaneRun(message)
		return nil
	case "lane_cancel":
		h.cancelLaneRun(message.RequestID)
		return nil
	case "lane_stdout", "lane_stderr", "lane_exit", "lane_error":
		h.deliverLaneResponse(message)
		return nil
	case "error":
		return errors.New(message.Error)
	default:
		return fmt.Errorf("unsupported hub frame %q", message.Type)
	}
}

// RemotePeers returns the current non-local roster sorted by global identity.
func (h *EmbeddedHost) RemotePeers() []Peer {
	h.mu.RLock()
	peers := clonePeerMap(h.remote)
	h.mu.RUnlock()
	return peers
}

// RemoteHosts returns the current connected non-local host roster.
func (h *EmbeddedHost) RemoteHosts() []Host {
	h.mu.RLock()
	hosts := make([]Host, 0, len(h.remoteHosts))
	for _, host := range h.remoteHosts {
		host.Capabilities = append([]string(nil), host.Capabilities...)
		hosts = append(hosts, host)
	}
	h.mu.RUnlock()
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	return hosts
}

// Connected reports whether the host currently owns a live hub stream.
func (h *EmbeddedHost) Connected() bool {
	h.mu.RLock()
	connected := h.network != nil
	h.mu.RUnlock()
	return connected
}

// Send delivers one already-resolved grouped message and waits for the remote
// daemon acknowledgement.
func (h *EmbeddedHost) Send(ctx context.Context, source, target Peer, messageID, content, group string) error {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(content) == "" {
		return errors.New("federated delivery requires message id and content")
	}
	if err := h.refreshLocal(ctx); err != nil {
		return err
	}
	h.mu.RLock()
	currentSource, sourceOK := h.local[source.ID]
	currentTarget, targetOK := h.remote[target.ID]
	h.mu.RUnlock()
	if !sourceOK || currentSource.InstanceID != source.InstanceID {
		return errors.New("federated source is no longer locally advertised")
	}
	if !targetOK || currentTarget.InstanceID != target.InstanceID {
		return errors.New("federated target is no longer live")
	}
	frame := DeliveryFrame(AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: messageID, Content: content, Group: group,
	}, currentSource)
	if err := validateDeliveryGroups(currentSource, currentTarget, frame); err != nil {
		return err
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return h.sendRemoteDelivery(ctx, Message{
		Type: "group_deliver", SourceID: currentSource.ID, TargetID: currentTarget.ID, Frame: body,
	})
}

func (h *EmbeddedHost) sendRemoteDelivery(ctx context.Context, message Message) error {
	requestID, err := randomRequestID(h.options.HostID)
	if err != nil {
		return err
	}
	message.RequestID = requestID
	done := make(chan deliveryOutcome, 1)
	h.deliveryMu.Lock()
	h.pendingDeliveries[requestID] = done
	h.deliveryMu.Unlock()
	defer func() {
		h.deliveryMu.Lock()
		delete(h.pendingDeliveries, requestID)
		h.deliveryMu.Unlock()
	}()
	h.mu.RLock()
	wire := h.network
	h.mu.RUnlock()
	if wire == nil {
		return errors.New("hub is disconnected")
	}
	if err := wire.Send(message); err != nil {
		return err
	}
	timer := time.NewTimer(wireWriteTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outcome := <-done:
		return outcome.err
	case <-timer.C:
		return errors.New("remote delivery acknowledgement timed out")
	}
}

func (h *EmbeddedHost) deliverInbound(message Message) error {
	if message.RequestID == "" || message.SourceID == "" || message.TargetID == "" {
		return errors.New("federated delivery is missing routing identity")
	}
	source, target, frame, err := h.resolveInboundDelivery(message)
	if err != nil {
		return err
	}
	frame.Source = ptrPeer(source)
	return h.options.Deliver(context.Background(), source, target, frame)
}

func (h *EmbeddedHost) resolveInboundDelivery(message Message) (Peer, Peer, AgentFrame, error) {
	if err := h.refreshLocal(context.Background()); err != nil {
		return Peer{}, Peer{}, AgentFrame{}, err
	}
	h.mu.RLock()
	target, targetOK := h.local[message.TargetID]
	source, sourceOK := h.remote[message.SourceID]
	h.mu.RUnlock()
	var frame AgentFrame
	if json.Unmarshal(message.Frame, &frame) != nil || frame.Version != AgentFrameVersion || frame.Type != "delivery" || frame.Source == nil {
		return Peer{}, Peer{}, AgentFrame{}, errors.New("invalid federated agent frame")
	}
	if message.Type == "terminal_notice_deliver" {
		source = clonePeer(*frame.Source)
		sourceOK = validateWirePeer(source, source.HostID) == nil && source.HostID != h.options.HostID
	}
	if !targetOK || !sourceOK || frame.Source.ID != source.ID || frame.SourceSessionID != source.SessionID {
		return Peer{}, Peer{}, AgentFrame{}, errors.New("federated source or local target is no longer live")
	}
	if err := validateDeliveryGroups(source, target, frame); err != nil {
		return Peer{}, Peer{}, AgentFrame{}, err
	}
	return source, target, frame, nil
}

func (h *EmbeddedHost) sendDeliveryOutcome(request Message, deliveryErr error) {
	h.mu.RLock()
	wire := h.network
	h.mu.RUnlock()
	if wire == nil || request.RequestID == "" {
		return
	}
	result := Message{
		Type: "delivery_ack", RequestID: request.RequestID, SourceID: request.SourceID, TargetID: request.TargetID,
	}
	if deliveryErr != nil {
		result.Type, result.Error = "delivery_error", deliveryErr.Error()
	}
	_ = wire.Send(result)
}

func (h *EmbeddedHost) completePendingDelivery(message Message) {
	h.deliveryMu.Lock()
	pending := h.pendingDeliveries[message.RequestID]
	h.deliveryMu.Unlock()
	if pending == nil {
		return
	}
	if message.Type == "delivery_error" {
		pending <- deliveryOutcome{err: errors.New(defaultString(message.Error, "remote delivery failed"))}
	} else {
		pending <- deliveryOutcome{}
	}
}

func (h *EmbeddedHost) failPendingDeliveries(reason string) {
	h.deliveryMu.Lock()
	for id, pending := range h.pendingDeliveries {
		delete(h.pendingDeliveries, id)
		select {
		case pending <- deliveryOutcome{err: errors.New(reason)}:
		default:
		}
	}
	h.deliveryMu.Unlock()
}

// RunRemoteLane sends one daemon-native lane operation to a connected host and
// returns its bounded structured result.
func (h *EmbeddedHost) RunRemoteLane(ctx context.Context, request RemoteLaneRequest) (RemoteLaneResult, error) {
	message, pending, err := h.startRemoteLane(ctx, request)
	if err != nil {
		return RemoteLaneResult{}, err
	}
	defer h.removePendingLane(message.RequestID)
	return h.collectRemoteLane(ctx, message, pending)
}

func (h *EmbeddedHost) startRemoteLane(ctx context.Context, request RemoteLaneRequest) (Message, *pendingLane, error) {
	if len(request.Input) > maxLaneInputBytes || len(request.Arguments) == 0 {
		return Message{}, nil, errors.New("remote lane request exceeds bounds or has no command")
	}
	if err := validateRemoteLaneArgs(request.Arguments); err != nil {
		return Message{}, nil, err
	}
	if err := h.refreshLocal(ctx); err != nil {
		return Message{}, nil, err
	}
	h.mu.RLock()
	source, sourceOK := h.local[request.Source.ID]
	h.mu.RUnlock()
	if !sourceOK || source.InstanceID != request.Source.InstanceID {
		return Message{}, nil, errors.New("remote lane source is no longer locally advertised")
	}
	capability, err := laneCapabilityForMessage(Message{
		Product: request.Product, Capabilities: []string{request.Capability},
	})
	if err != nil {
		return Message{}, nil, err
	}
	host, err := h.resolveRemoteHost(request.TargetHostID, capability)
	if err != nil {
		return Message{}, nil, err
	}
	requestID := ""
	if request.IdempotencyKey != "" {
		requestID = stableRemoteLaneRequestID(h.options.HostID, request.IdempotencyKey)
	} else {
		requestID, err = randomRequestID(h.options.HostID)
		if err != nil {
			return Message{}, nil, err
		}
	}
	pending := &pendingLane{responses: make(chan Message, 256), failed: make(chan string, 1)}
	h.laneMu.Lock()
	h.pendingLanes[requestID] = pending
	h.laneMu.Unlock()
	message := Message{
		Type: "lane_exec", RequestID: requestID, SourceID: source.ID, TargetHostID: host.ID,
		Product: request.Product, Capabilities: []string{capability}, Args: append([]string(nil), request.Arguments...),
		Input: append([]byte(nil), request.Input...), ParentContext: ptrParent(request.Parent),
	}
	h.mu.RLock()
	wire := h.network
	h.mu.RUnlock()
	if wire == nil || wire.Send(message) != nil {
		h.removePendingLane(requestID)
		return Message{}, nil, errors.New("hub is disconnected")
	}
	return message, pending, nil
}

func (h *EmbeddedHost) removePendingLane(requestID string) {
	h.laneMu.Lock()
	delete(h.pendingLanes, requestID)
	h.laneMu.Unlock()
}

func (h *EmbeddedHost) collectRemoteLane(ctx context.Context, message Message, pending *pendingLane) (RemoteLaneResult, error) {
	var stdout, stderr bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			h.sendLaneCancel(message)
			return RemoteLaneResult{}, ctx.Err()
		case reason := <-pending.failed:
			return RemoteLaneResult{}, errors.New(reason)
		case response := <-pending.responses:
			result, done, err := h.consumeRemoteLaneResponse(message, response, &stdout, &stderr)
			if err != nil || done {
				return result, err
			}
		}
	}
}

func (h *EmbeddedHost) consumeRemoteLaneResponse(
	message, response Message,
	stdout, stderr *bytes.Buffer,
) (RemoteLaneResult, bool, error) {
	switch response.Type {
	case "lane_stdout":
		if err := writeRemoteLaneOutput(stdout, response.Data, "stdout"); err != nil {
			h.sendLaneCancel(message)
			return RemoteLaneResult{}, true, err
		}
	case "lane_stderr":
		if err := writeRemoteLaneOutput(stderr, response.Data, "stderr"); err != nil {
			h.sendLaneCancel(message)
			return RemoteLaneResult{}, true, err
		}
	case "lane_exit":
		return RemoteLaneResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: response.ExitCode}, true, nil
	case "lane_error":
		return RemoteLaneResult{}, true, errors.New(response.Error)
	}
	return RemoteLaneResult{}, false, nil
}

func writeRemoteLaneOutput(output *bytes.Buffer, data []byte, stream string) error {
	if output.Len()+len(data) > maxEmbeddedLaneOutput {
		return fmt.Errorf("remote lane %s exceeds 16 MiB", stream)
	}
	_, _ = output.Write(data)
	return nil
}

func (h *EmbeddedHost) resolveRemoteHost(target, capability string) (Host, error) {
	if capability == "" {
		return Host{}, errors.New("remote lane product is unsupported")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.network == nil {
		return Host{}, errors.New("hub is disconnected")
	}
	if host, exists := h.remoteHosts[target]; exists {
		if !contains(host.Capabilities, capability) {
			return Host{}, fmt.Errorf("remote host %s does not advertise %s", host.ID, capability)
		}
		return host, nil
	}
	matches := make([]Host, 0, 1)
	for _, host := range h.remoteHosts {
		if strings.EqualFold(host.Name, target) {
			matches = append(matches, host)
		}
	}
	if len(matches) == 0 {
		return Host{}, fmt.Errorf("remote host %q is not connected to the hub", target)
	}
	if len(matches) > 1 {
		return Host{}, fmt.Errorf("remote host name %q is ambiguous; use a host id", target)
	}
	if !contains(matches[0].Capabilities, capability) {
		return Host{}, fmt.Errorf("remote host %s does not advertise %s", matches[0].ID, capability)
	}
	return matches[0], nil
}

func (h *EmbeddedHost) sendLaneCancel(request Message) {
	h.mu.RLock()
	wire := h.network
	h.mu.RUnlock()
	if wire != nil {
		_ = wire.Send(Message{
			Type: "lane_cancel", RequestID: request.RequestID,
			SourceID: request.SourceID, TargetHostID: request.TargetHostID,
		})
	}
}

func (h *EmbeddedHost) deliverLaneResponse(message Message) {
	h.laneMu.Lock()
	pending := h.pendingLanes[message.RequestID]
	h.laneMu.Unlock()
	if pending == nil {
		return
	}
	select {
	case pending.responses <- message:
	default:
		select {
		case pending.failed <- "remote lane output exceeded the local proxy buffer":
			pending.cancelOnce.Do(func() { h.sendLaneCancel(message) })
		default:
		}
	}
}

func (h *EmbeddedHost) failPendingLanes(reason string) {
	h.laneMu.Lock()
	for _, pending := range h.pendingLanes {
		select {
		case pending.failed <- reason:
		default:
		}
	}
	h.laneMu.Unlock()
}

func (h *EmbeddedHost) startLaneRun(request Message) {
	h.laneMu.Lock()
	if _, exists := h.laneRuns[request.RequestID]; exists {
		h.laneMu.Unlock()
		_ = h.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "duplicate lane request"})
		return
	}
	if len(h.laneRuns) >= maxRemoteLaneRuns {
		h.laneMu.Unlock()
		_ = h.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "remote lane concurrency limit reached"})
		return
	}
	run := &laneRun{}
	h.laneRuns[request.RequestID] = run
	h.laneMu.Unlock()
	go h.runInboundLane(request, run)
}

func (h *EmbeddedHost) runInboundLane(request Message, run *laneRun) {
	defer func() {
		h.laneMu.Lock()
		delete(h.laneRuns, request.RequestID)
		h.laneMu.Unlock()
	}()
	h.mu.RLock()
	source, sourceOK := h.remote[request.SourceID]
	connected := h.network != nil
	h.mu.RUnlock()
	capability, capabilityErr := laneCapabilityForMessage(request)
	if !connected || !sourceOK || capabilityErr != nil || !contains(h.options.Capabilities, capability) ||
		h.options.RunLane == nil || request.ParentContext == nil || len(request.Args) == 0 || len(request.Input) > maxLaneInputBytes {
		_ = h.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "invalid or stale embedded remote lane request"})
		return
	}
	if !parentMatchesPeer(*request.ParentContext, source) {
		_ = h.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "remote lane parent context does not match its source peer"})
		return
	}
	if err := validateRemoteLaneArgs(request.Args); err != nil {
		_ = h.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.mu.Lock()
	if run.cancelled {
		run.mu.Unlock()
		cancel()
		_ = h.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "lane request was cancelled"})
		return
	}
	run.cancel = cancel
	run.mu.Unlock()
	result, err := h.options.RunLane(ctx, RemoteLaneRequest{
		Source: source, Parent: *request.ParentContext, TargetHostID: h.options.HostID,
		Product: request.Product, Capability: capability,
		Arguments: append([]string(nil), request.Args...), Input: append([]byte(nil), request.Input...),
		IdempotencyKey: request.RequestID,
	})
	cancel()
	if err != nil {
		_ = h.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	if err := h.sendLaneBytes(request.RequestID, "lane_stdout", result.Stdout); err != nil {
		return
	}
	if err := h.sendLaneBytes(request.RequestID, "lane_stderr", result.Stderr); err != nil {
		return
	}
	_ = h.sendLaneMessage(Message{Type: "lane_exit", RequestID: request.RequestID, ExitCode: result.ExitCode})
}

func (h *EmbeddedHost) sendLaneBytes(requestID, kind string, body []byte) error {
	for len(body) > 0 {
		count := 32 * 1024
		if len(body) < count {
			count = len(body)
		}
		if err := h.sendLaneMessage(Message{Type: kind, RequestID: requestID, Data: append([]byte(nil), body[:count]...)}); err != nil {
			return err
		}
		body = body[count:]
	}
	return nil
}

func (h *EmbeddedHost) sendLaneMessage(message Message) error {
	h.mu.RLock()
	wire := h.network
	h.mu.RUnlock()
	if wire == nil {
		return errors.New("hub is disconnected")
	}
	return wire.Send(message)
}

func (h *EmbeddedHost) cancelLaneRun(requestID string) {
	h.laneMu.Lock()
	run := h.laneRuns[requestID]
	h.laneMu.Unlock()
	if run != nil {
		run.stop()
	}
}

func (h *EmbeddedHost) cancelAllLaneRuns() {
	h.laneMu.Lock()
	runs := make([]*laneRun, 0, len(h.laneRuns))
	for _, run := range h.laneRuns {
		runs = append(runs, run)
	}
	h.laneMu.Unlock()
	for _, run := range runs {
		run.stop()
	}
}

func (r *laneRun) stop() {
	r.mu.Lock()
	r.cancelled = true
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func parentMatchesPeer(parent ParentContext, peer Peer) bool {
	product := peer.Product
	if product == "" {
		product = peer.Entrypoint
	}
	return parent.HostID == peer.HostID && parent.SessionID == peer.SessionID &&
		parent.Product == product && parent.InstanceID == peer.InstanceID &&
		reflect.DeepEqual(sortedCopy(parent.Groups), sortedCopy(peer.Groups))
}

func validateRemoteLaneArgs(args []string) error {
	if len(args) == 0 || len(args) > maxRemoteLaneArgs {
		return errors.New("remote lane argument count is out of bounds")
	}
	total := 0
	for _, argument := range args {
		if strings.IndexByte(argument, 0) >= 0 {
			return errors.New("remote lane argument contains NUL")
		}
		total += len(argument)
		if total > maxRemoteLaneArgBytes {
			return errors.New("remote lane arguments exceed 512 KiB")
		}
	}
	return nil
}

func validateWirePeer(peer Peer, hostID string) error {
	product := peer.Product
	if product == "" {
		product = peer.Entrypoint
	}
	if len(hostID) > maxHostIDBytes || len(peer.ID) > maxPeerIDBytes || len(peer.GlobalID) > maxPeerGlobalIDBytes ||
		len(peer.Name) > maxPeerNameBytes || len(peer.DisplayName) > maxPeerDisplayNameBytes ||
		len(peer.HostID) > maxHostIDBytes || len(peer.HostName) > maxHostNameBytes ||
		len(peer.Product) > maxProductTokenBytes || len(peer.Entrypoint) > maxProductTokenBytes ||
		len(peer.Status) > maxPeerStatusBytes || len(peer.Cwd) > maxPeerCwdBytes ||
		len(peer.PermissionMode) > maxPeerPermissionBytes || len(peer.InstanceID) > maxPeerInstanceIDBytes ||
		len(peer.ParentSessionID) > maxPeerParentIDBytes || len(peer.Groups) > maxPeerGroups {
		return errors.New("snapshot peer fields exceed protocol bounds")
	}
	if peer.HostID != hostID || !validSimpleID(peer.HostID) || !validSessionID(peer.SessionID) ||
		peer.ID != hostID+"/"+peer.SessionID || peer.GlobalID != globalSessionID(hostID, peer.SessionID) {
		return errors.New("snapshot contains an invalid peer identity")
	}
	if peer.PeerProtocol != GroupProtocolVersion {
		return errors.New("snapshot contains an incompatible grouped peer")
	}
	if productcatalog.ValidateToken(product) != nil ||
		(peer.Product != "" && productcatalog.ValidateToken(peer.Product) != nil) ||
		(peer.Entrypoint != "" && productcatalog.ValidateToken(peer.Entrypoint) != nil) ||
		strings.TrimSpace(peer.Name) == "" || strings.TrimSpace(peer.InstanceID) == "" {
		return errors.New("snapshot contains an invalid product peer")
	}
	if len(peer.Groups) == 0 || !contains(peer.Groups, PrivateGroup(hostID, peer.SessionID)) {
		return errors.New("snapshot contains invalid peer groups")
	}
	seen := map[string]bool{}
	for _, group := range peer.Groups {
		if err := validateEffectiveGroup(group); err != nil || seen[group] {
			return errors.New("snapshot contains invalid peer groups")
		}
		seen[group] = true
	}
	return nil
}

func validateRosterHost(host Host) error {
	if !validSimpleID(host.ID) || len(host.ID) > maxHostIDBytes ||
		strings.TrimSpace(host.Name) == "" || len(host.Name) > maxHostNameBytes || len(host.Build) > maxBuildBytes {
		return errors.New("host identity fields exceed protocol bounds")
	}
	return nil
}

func validateLocalPeer(peer Peer, hostID string) error {
	if err := validateWirePeer(peer, hostID); err != nil {
		return err
	}
	product := peer.Product
	if product == "" {
		product = peer.Entrypoint
	}
	if _, ok := productcatalog.ByID(product); !ok {
		return errors.New("snapshot contains a product outside the local catalog")
	}
	return nil
}

func validateDeliveryGroups(source, target Peer, frame AgentFrame) error {
	if frame.Group != "" {
		if !contains(source.Groups, frame.Group) || !contains(target.Groups, frame.Group) {
			return errors.New("federated broadcast peers are not current members of the named group")
		}
		return nil
	}
	if !groupsIntersect(source.Groups, target.Groups) {
		return errors.New("federated peers do not share a group")
	}
	return nil
}

// BuildPeer creates the protocol projection for one daemon attachment or lane
// and adds its mandatory private host/session anchor.
func BuildPeer(hostID, hostName, sessionID, name, status, cwd, product, permission, instanceID, parentID string, groups []string) (Peer, error) {
	if !validSimpleID(hostID) || !validSessionID(sessionID) {
		return Peer{}, errors.New("invalid embedded peer identity")
	}
	if _, ok := productcatalog.ByID(product); !ok {
		return Peer{}, errors.New("invalid embedded peer product")
	}
	if strings.TrimSpace(hostName) == "" {
		hostName = hostID
	}
	if strings.TrimSpace(name) == "" {
		name = sessionID
	}
	if strings.TrimSpace(instanceID) == "" {
		instanceID = product + ":" + sessionID
	}
	effective, err := effectivePeerGroups(hostID, sessionID, groups)
	if err != nil {
		return Peer{}, err
	}
	peer := Peer{
		ID: hostID + "/" + sessionID, HostID: hostID, HostName: hostName,
		SessionID: sessionID, GlobalID: globalSessionID(hostID, sessionID),
		Name: name, DisplayName: qualifiedName(name, hostName), Product: product, Entrypoint: product,
		Status: status, Cwd: cwd, PermissionMode: permission, PeerProtocol: GroupProtocolVersion,
		InstanceID: instanceID, Groups: effective, ParentSessionID: parentID,
	}
	if err := validateLocalPeer(peer, hostID); err != nil {
		return Peer{}, err
	}
	return peer, nil
}

func effectivePeerGroups(hostID, sessionID string, groups []string) ([]string, error) {
	anchor := PrivateGroup(hostID, sessionID)
	values := append([]string(nil), groups...)
	if !contains(values, anchor) {
		values = append(values, anchor)
	}
	sort.Strings(values)
	seen := map[string]bool{}
	for _, group := range values {
		if err := validateEffectiveGroup(group); err != nil || seen[group] {
			return nil, errors.New("embedded peer groups are invalid or duplicated")
		}
		seen[group] = true
	}
	return values, nil
}

func randomRequestID(hostID string) (string, error) {
	body := make([]byte, 12)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return hostID + "-" + hex.EncodeToString(body), nil
}

func stableRemoteLaneRequestID(hostID, key string) string {
	digest := sha256.Sum256([]byte(hostID + "\x00" + key))
	return hostID + "-" + hex.EncodeToString(digest[:12])
}

func globalSessionID(hostID, sessionID string) string {
	value := sessionkey.FromID(hostID+"\x00"+sessionID) + "_" + cleanToken(hostID) + "_" + cleanToken(sessionID)
	if len(value) > 100 {
		return value[:100]
	}
	return value
}

func qualifiedName(name, host string) string {
	return cleanPeerName(defaultString(strings.TrimSpace(name), "peer")) + "--" + cleanPeerName(defaultString(strings.TrimSpace(host), "host"))
}

func cleanPeerName(value string) string {
	var output strings.Builder
	separator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("._-", r) {
			output.WriteRune(r)
			separator = false
		} else if !separator {
			output.WriteByte('-')
			separator = true
		}
		if output.Len() >= 72 {
			break
		}
	}
	return defaultString(strings.Trim(output.String(), "._-"), "peer")
}

func cleanToken(value string) string {
	var output strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			output.WriteRune(r)
		}
		if output.Len() >= 48 {
			break
		}
	}
	return strings.Trim(output.String(), "_-")
}

func clonePeerMap(source map[string]Peer) []Peer {
	peers := make([]Peer, 0, len(source))
	for _, peer := range source {
		peers = append(peers, clonePeer(peer))
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	return peers
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func ptrPeer(peer Peer) *Peer { return &peer }

func ptrParent(parent ParentContext) *ParentContext {
	parent.Groups = append([]string(nil), parent.Groups...)
	return &parent
}

func defaultLogger(logger *log.Logger) *log.Logger {
	if logger != nil {
		return logger
	}
	return log.New(os.Stderr, "agent-sessions-federation: ", log.LstdFlags|log.Lmicroseconds)
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
