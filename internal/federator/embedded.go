package federator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"time"
)

const maxEmbeddedLaneOutput = 16 << 20

// EmbeddedHostOptions connects the unified user daemon to the existing
// protocol-3 hub without starting a second host-agent process. Local product
// ownership stays behind the callbacks; this package owns only federation.
type EmbeddedHostOptions struct {
	Hub               string
	HostID            string
	HostName          string
	Capabilities      []string
	ScanInterval      time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	Snapshot          func(context.Context) ([]Peer, error)
	Deliver           func(context.Context, Peer, Peer, AgentFrame) error
	RunLane           func(context.Context, RemoteLaneRequest) (RemoteLaneResult, error)
	Logger            *log.Logger
}

// RemoteLaneRequest is one hub-attested lane operation delivered to the
// destination daemon.
type RemoteLaneRequest struct {
	Source       Peer
	Parent       ParentContext
	TargetHostID string
	Product      string
	Arguments    []string
	Input        []byte
}

// RemoteLaneResult is the bounded stdout/stderr/exit projection returned over
// the existing lane stream protocol.
type RemoteLaneResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type embeddedBackend struct {
	snapshot     func(context.Context) ([]Peer, error)
	deliver      func(context.Context, Peer, Peer, AgentFrame) error
	runLane      func(context.Context, RemoteLaneRequest) (RemoteLaneResult, error)
	capabilities []string
}

// EmbeddedHost is the daemon-owned federation component. It is safe for
// concurrent roster queries, deliveries, and remote lane requests.
type EmbeddedHost struct{ agent *agent }

// NewEmbeddedHost validates and constructs the daemon-owned host component.
func NewEmbeddedHost(options EmbeddedHostOptions) (*EmbeddedHost, error) {
	hostID := cleanID(options.HostID)
	if hostID == "" || hostID != options.HostID {
		return nil, errors.New("embedded federation host id must be a simple stable identifier")
	}
	if strings.TrimSpace(options.HostName) == "" {
		options.HostName = hostID
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
	capabilities := normalizeCapabilities(options.Capabilities)
	if len(capabilities) != 0 && options.RunLane == nil {
		return nil, errors.New("embedded federation lane capabilities require a lane callback")
	}
	backend := &embeddedBackend{
		snapshot: options.Snapshot, deliver: options.Deliver, runLane: options.RunLane,
		capabilities: capabilities,
	}
	return &EmbeddedHost{agent: &agent{
		options: AgentOptions{
			Hub: options.Hub, HostID: hostID, HostName: options.HostName,
			ScanInterval: options.ScanInterval, HeartbeatInterval: options.HeartbeatInterval,
			HeartbeatTimeout: options.HeartbeatTimeout, EnableRemoteLanes: len(capabilities) != 0,
		},
		logger: defaultLogger(options.Logger), local: map[string]localPeer{},
		remote: map[string]Peer{}, remoteHosts: map[string]Host{},
		pendingLanes: map[string]*pendingLane{}, laneRuns: map[string]*laneRun{},
		pendingDeliveries: map[string]chan error{}, localChanged: make(chan struct{}, 1),
		embedded: backend,
	}}, nil
}

// Run maintains the hub connection and current daemon snapshot until ctx is
// canceled. Hub outages reconnect with the same bounded legacy backoff.
func (h *EmbeddedHost) Run(ctx context.Context) error { return h.agent.run(ctx) }

func (a *agent) runEmbedded(ctx context.Context) error {
	if err := a.refreshLocal(); err != nil {
		return err
	}
	go a.localDiscoveryLoop(ctx)
	defer func() {
		a.setNetwork(nil)
		a.clearRemotePeers()
	}()
	if a.options.Hub == "" {
		<-ctx.Done()
		return nil
	}
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := a.runHubSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		a.logger.Printf("hub session ended: %v", err)
		a.setNetwork(nil)
		a.clearRemotePeers()
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

func (a *agent) refreshEmbeddedLocal() error {
	peers, err := a.embedded.snapshot(context.Background())
	if err != nil {
		return err
	}
	next := make(map[string]localPeer, len(peers))
	for _, peer := range peers {
		if err := validateGroupedSnapshotPeer(peer, a.options.HostID); err != nil {
			return fmt.Errorf("invalid embedded peer %s: %w", peer.ID, err)
		}
		if _, exists := next[peer.ID]; exists {
			return fmt.Errorf("duplicate embedded peer %s", peer.ID)
		}
		next[peer.ID] = localPeer{Peer: peer}
	}
	a.mu.Lock()
	changed := !reflect.DeepEqual(a.local, next)
	a.local = next
	a.mu.Unlock()
	if changed {
		a.signalLocalChanged()
	}
	return nil
}

// BuildPeer creates the exact protocol projection for one daemon attachment
// or lane and adds its mandatory private host/session anchor.
func BuildPeer(hostID, hostName, sessionID, name, status, cwd, product, permission, instanceID, parentID string, groups []string) (Peer, error) {
	rawHostID := hostID
	hostID = cleanID(hostID)
	if hostID == "" || hostID != rawHostID || !validCatalogSessionID(sessionID) || !validProduct(product) {
		return Peer{}, errors.New("invalid embedded peer identity")
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
	effective, err := normalizeEffectiveGroups(sortedUnique(append(append([]string(nil), groups...), privateGroupPrefix+hostID+"/"+sessionID)))
	if err != nil {
		return Peer{}, err
	}
	peer := Peer{
		ID: hostID + "/" + sessionID, HostID: hostID, HostName: hostName,
		SessionID: sessionID, GlobalID: globalSessionID(hostID, sessionID),
		Name: name, DisplayName: qualifiedName(name, hostName), Status: status, Cwd: cwd,
		Entrypoint: product, PermissionMode: permission, PeerProtocol: GroupProtocolVersion,
		InstanceID: instanceID, Groups: effective, ParentSessionID: parentID,
	}
	if err := validateGroupedSnapshotPeer(peer, hostID); err != nil {
		return Peer{}, err
	}
	return peer, nil
}

// RemotePeers returns the current non-local roster sorted by global identity.
func (h *EmbeddedHost) RemotePeers() []Peer {
	h.agent.mu.RLock()
	peers := make([]Peer, 0, len(h.agent.remote))
	for _, peer := range h.agent.remote {
		peer.Groups = append([]string(nil), peer.Groups...)
		peers = append(peers, peer)
	}
	h.agent.mu.RUnlock()
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	return peers
}

// RemoteHosts returns the current connected non-local host roster.
func (h *EmbeddedHost) RemoteHosts() []Host {
	hosts, _ := h.agent.remoteHostSnapshot()
	return hosts
}

// Send delivers one already-resolved grouped message and waits for the
// destination daemon acknowledgement.
func (h *EmbeddedHost) Send(ctx context.Context, source, target Peer, messageID, content, group string) error {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(content) == "" {
		return errors.New("federated delivery requires message id and content")
	}
	if err := h.agent.refreshLocal(); err != nil {
		return err
	}
	h.agent.mu.RLock()
	currentSource, sourceOK := h.agent.local[source.ID]
	currentTarget, targetOK := h.agent.remote[target.ID]
	h.agent.mu.RUnlock()
	if !sourceOK || currentSource.InstanceID != source.InstanceID {
		return errors.New("federated source is no longer locally advertised")
	}
	if !targetOK || currentTarget.InstanceID != target.InstanceID {
		return errors.New("federated target is no longer live")
	}
	frame := AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: messageID,
		SourceSessionID: currentSource.SessionID, Source: &currentSource.Peer,
		Content: content, Group: group, SentAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := validateCurrentDeliveryGroups(currentSource.Peer, currentTarget, frame); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- h.agent.deliverAgentFrameRemote(currentSource.Peer, currentTarget, frame) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// RunRemoteLane sends one daemon-native lane operation to a connected host and
// returns its bounded structured result.
//
//nolint:gocyclo // The existing streamed lane protocol has explicit validation, cancellation, and bounded collection states.
func (h *EmbeddedHost) RunRemoteLane(ctx context.Context, request RemoteLaneRequest) (RemoteLaneResult, error) {
	if len(request.Input) > maxLaneInputBytes || len(request.Arguments) == 0 {
		return RemoteLaneResult{}, errors.New("remote lane request exceeds bounds or has no command")
	}
	if err := validateRemoteLaneArgBounds(request.Arguments); err != nil {
		return RemoteLaneResult{}, err
	}
	if err := h.agent.refreshLocal(); err != nil {
		return RemoteLaneResult{}, err
	}
	h.agent.mu.RLock()
	local, sourceOK := h.agent.local[request.Source.ID]
	h.agent.mu.RUnlock()
	if !sourceOK || local.InstanceID != request.Source.InstanceID {
		return RemoteLaneResult{}, errors.New("remote lane source is no longer locally advertised")
	}
	host, err := h.agent.resolveRemoteHost(request.TargetHostID, capabilityForProduct(request.Product))
	if err != nil {
		return RemoteLaneResult{}, err
	}
	requestID, err := randomLaneRequestID(h.agent.options.HostID)
	if err != nil {
		return RemoteLaneResult{}, err
	}
	pending := &pendingLane{responses: make(chan Message, 256), failed: make(chan string, 1)}
	h.agent.laneMu.Lock()
	h.agent.pendingLanes[requestID] = pending
	h.agent.laneMu.Unlock()
	defer func() {
		h.agent.laneMu.Lock()
		delete(h.agent.pendingLanes, requestID)
		h.agent.laneMu.Unlock()
	}()
	message := Message{
		Type: "lane_exec", RequestID: requestID, SourceID: local.ID, TargetHostID: host.ID,
		Product: request.Product, Args: append([]string(nil), request.Arguments...),
		Input: append([]byte(nil), request.Input...), ParentContext: &request.Parent,
	}
	h.agent.mu.RLock()
	wire := h.agent.network
	h.agent.mu.RUnlock()
	if wire == nil || wire.Send(message) != nil {
		return RemoteLaneResult{}, errors.New("hub is disconnected")
	}
	var stdout, stderr bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			h.agent.sendLaneCancel(message)
			return RemoteLaneResult{}, ctx.Err()
		case reason := <-pending.failed:
			return RemoteLaneResult{}, errors.New(reason)
		case response := <-pending.responses:
			switch response.Type {
			case "lane_stdout":
				if stdout.Len()+len(response.Data) > maxEmbeddedLaneOutput {
					h.agent.sendLaneCancel(message)
					return RemoteLaneResult{}, errors.New("remote lane stdout exceeds 16 MiB")
				}
				_, _ = stdout.Write(response.Data)
			case "lane_stderr":
				if stderr.Len()+len(response.Data) > maxEmbeddedLaneOutput {
					h.agent.sendLaneCancel(message)
					return RemoteLaneResult{}, errors.New("remote lane stderr exceeds 16 MiB")
				}
				_, _ = stderr.Write(response.Data)
			case "lane_exit":
				return RemoteLaneResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: response.ExitCode}, nil
			case "lane_error":
				return RemoteLaneResult{}, errors.New(response.Error)
			}
		}
	}
}

//nolint:gocyclo // Destination streaming validates every request field and emits one bounded protocol result.
func (a *agent) runEmbeddedRemoteLane(request Message, run *laneRun) {
	a.mu.RLock()
	source, sourceOK := a.remote[request.SourceID]
	connected := a.network != nil
	a.mu.RUnlock()
	capability := capabilityForProduct(request.Product)
	if !connected || !sourceOK || capability == "" || !containsString(a.embedded.capabilities, capability) ||
		request.ParentContext == nil || len(request.Args) == 0 || len(request.Input) > maxLaneInputBytes {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "invalid or stale embedded remote lane request"})
		return
	}
	if request.ParentContext.HostID != source.HostID || request.ParentContext.SessionID != source.SessionID ||
		request.ParentContext.Product != source.Entrypoint || request.ParentContext.InstanceID != source.InstanceID ||
		!reflect.DeepEqual(sortedUnique(request.ParentContext.Groups), sortedUnique(source.Groups)) {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "remote lane parent context does not match its source peer"})
		return
	}
	if err := validateRemoteLaneArgBounds(request.Args); err != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.mu.Lock()
	if run.cancelled {
		run.mu.Unlock()
		cancel()
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: "lane request was cancelled"})
		return
	}
	run.cancel = cancel
	run.mu.Unlock()
	result, err := a.embedded.runLane(ctx, RemoteLaneRequest{
		Source: source, Parent: *request.ParentContext, TargetHostID: a.options.HostID,
		Product: request.Product, Arguments: append([]string(nil), request.Args...), Input: append([]byte(nil), request.Input...),
	})
	cancel()
	if err != nil {
		_ = a.sendLaneMessage(Message{Type: "lane_error", RequestID: request.RequestID, Error: err.Error()})
		return
	}
	if err := a.sendEmbeddedLaneBytes(request.RequestID, "lane_stdout", result.Stdout); err != nil {
		return
	}
	if err := a.sendEmbeddedLaneBytes(request.RequestID, "lane_stderr", result.Stderr); err != nil {
		return
	}
	_ = a.sendLaneMessage(Message{Type: "lane_exit", RequestID: request.RequestID, ExitCode: result.ExitCode})
}

func (a *agent) sendEmbeddedLaneBytes(requestID, kind string, body []byte) error {
	for len(body) > 0 {
		count := 32 * 1024
		if len(body) < count {
			count = len(body)
		}
		if err := a.sendLaneMessage(Message{Type: kind, RequestID: requestID, Data: append([]byte(nil), body[:count]...)}); err != nil {
			return err
		}
		body = body[count:]
	}
	return nil
}
