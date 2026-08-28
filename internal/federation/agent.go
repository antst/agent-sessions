package federation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAgentDialTimeout       = 5 * time.Second
	defaultAgentHandshakeTimeout  = 5 * time.Second
	defaultAgentWriteTimeout      = 10 * time.Second
	defaultAgentHeartbeatInterval = 5 * time.Second
	defaultAgentHeartbeatTimeout  = 20 * time.Second
	defaultAgentDeliveryTimeout   = 30 * time.Second
	defaultAgentInitialBackoff    = 250 * time.Millisecond
	defaultAgentMaximumBackoff    = 5 * time.Second
)

// AgentConnectionState is the bounded lifecycle of one embedded host-to-hub
// connection. It contains no product, message, prompt, result, or credential
// content.
type AgentConnectionState string

const (
	// AgentDisabled identifies a host with no configured hub.
	AgentDisabled AgentConnectionState = "disabled"
	// AgentConnecting identifies an outbound connection or handshake attempt.
	AgentConnecting AgentConnectionState = "connecting"
	// AgentConnected identifies a protocol-matching, handshaken connection.
	AgentConnected AgentConnectionState = "connected"
	// AgentBackoff identifies a bounded retry delay after a transport failure.
	AgentBackoff AgentConnectionState = "backoff"
	// AgentIncompatible identifies a peer which declared another protocol.
	AgentIncompatible AgentConnectionState = "incompatible"
)

// AgentStatus is a metadata-only snapshot of the embedded host connection.
type AgentStatus struct {
	HostID                 string
	HostName               string
	HubAddress             string
	State                  AgentConnectionState
	ProtocolVersion        int
	Capabilities           []string
	ConnectionGeneration   uint64
	RemoteRosterGeneration uint64
	LastConnectedAt        int64
	LastErrorCode          string
}

// Roster is the hub-owned remote host and peer projection.
type Roster struct {
	Hosts []Host
	Peers []Peer
}

// RoutedDelivery is one typed grouped message or terminal notice delivered by
// the hub. Frame content is passed only to the in-process routing callback and
// is never retained in connection status.
type RoutedDelivery struct {
	Type      string
	RequestID string
	SourceID  string
	TargetID  string
	Frame     json.RawMessage
}

// DeliveryOutcome is the correlated metadata-only result of an outbound
// grouped delivery.
type DeliveryOutcome struct {
	RequestID string
	SourceID  string
	TargetID  string
	Error     string
}

// ParentContext is the product-neutral parent identity attested for a remote
// lane request. Process fields are evidence, not an additional authority.
type ParentContext struct {
	HostID               string   `json:"host_id"`
	SessionID            string   `json:"session_id"`
	Product              string   `json:"product"`
	InstanceID           string   `json:"instance_id"`
	Groups               []string `json:"groups"`
	AlwaysApprove        bool     `json:"always_approve"`
	AgentRuntimeDir      string   `json:"agent_runtime_dir,omitempty"`
	AdapterPID           int      `json:"adapter_pid,omitempty"`
	AdapterProcStart     string   `json:"adapter_proc_start,omitempty"`
	AdapterStrongStart   string   `json:"adapter_strong_start,omitempty"`
	AdapterSocket        string   `json:"adapter_socket,omitempty"`
	PID                  int      `json:"pid"`
	ProcStart            string   `json:"proc_start"`
	StrongStart          string   `json:"strong_start,omitempty"`
	PermissionMode       string   `json:"permission_mode"`
	QwenCapabilityDigest string   `json:"qwen_capability_digest,omitempty"`
}

// RemoteLaneWireRequest is one typed legacy wire operation relayed by the
// hub. It remains distinct from the daemon's normalized durable lane request.
type RemoteLaneWireRequest struct {
	RequestID    string
	SourceID     string
	TargetHostID string
	Product      string
	Args         []string
	Input        []byte
	Parent       *ParentContext
}

// RemoteLaneCancellation identifies one accepted remote request to cancel.
type RemoteLaneCancellation struct{ RequestID string }

// RemoteLaneResponse is one typed result fragment relayed by the hub.
type RemoteLaneResponse struct {
	Type      string
	RequestID string
	Data      []byte
	ExitCode  int
	Error     string
	Result    *RemoteLaneResult
}

// AgentCallbacks connects wire events directly to the daemon's in-process
// authorities. A nil work callback rejects that work; it never falls back to
// another local process or socket.
type AgentCallbacks struct {
	Snapshot       func(context.Context) ([]Peer, error)
	Roster         func(context.Context, Roster) error
	GroupDelivery  func(context.Context, RoutedDelivery) error
	TerminalNotice func(context.Context, RoutedDelivery) error
	DeliveryResult func(context.Context, DeliveryOutcome) error
	LaneRequest    func(context.Context, RemoteLaneWireRequest) error
	RemoteLane     func(context.Context, RemoteLaneEnvelope) (RemoteLaneAccepted, error)
	LaneCancel     func(context.Context, RemoteLaneCancellation) error
	LaneArchive    func(context.Context, RemoteLaneArchive) (RemoteLaneArchived, error)
	LaneResponse   func(context.Context, RemoteLaneResponse) error
	StateChanged   func(AgentStatus)
}

// DialContextFunc makes the outbound transport injectable without moving
// connection ownership outside the host daemon.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// HostAgentOptions configures one embedded host connection authority.
type HostAgentOptions struct {
	HubAddress        string
	HostID            string
	HostName          string
	Capabilities      []string
	Advertisement     HostAdvertisement
	Callbacks         AgentCallbacks
	DialContext       DialContextFunc
	Now               func() time.Time
	DialTimeout       time.Duration
	HandshakeTimeout  time.Duration
	WriteTimeout      time.Duration
	DeliveryTimeout   time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	InitialBackoff    time.Duration
	MaximumBackoff    time.Duration
}

// ProtocolMismatchError reports the only software-interoperability failure.
// Release and build identities are deliberately absent.
type ProtocolMismatchError struct {
	Expected int
	Received int
}

func (failure *ProtocolMismatchError) Error() string {
	return fmt.Sprintf("federation protocol mismatch: requires matching version %d, received %d", failure.Expected, failure.Received)
}

// HostAgent owns one serial connection/retry loop for a daemon generation.
type HostAgent struct {
	options HostAgentOptions

	mu      sync.RWMutex
	status  AgentStatus
	wire    *agentWireConn
	running bool

	snapshotChanged chan struct{}
	pendingMu       sync.Mutex
	pendingDelivery map[string]chan error
	pendingLane     map[string]chan remoteLaneAdmission
	pendingCancel   map[string]chan error
	pendingArchive  map[string]chan remoteLaneArchiveDecision
	pendingResult   map[string]chan error
}

type remoteLaneAdmission struct {
	accepted RemoteLaneAccepted
	err      error
}

type remoteLaneArchiveDecision struct {
	archived RemoteLaneArchived
	err      error
}

type remoteLaneCancellationRefusedError struct{ detail string }

func (failure *remoteLaneCancellationRefusedError) Error() string {
	return "remote lane cancellation was refused: " + failure.detail
}

// IsRemoteLaneCancellationRefused reports a destination-owned cancellation refusal.
func IsRemoteLaneCancellationRefused(err error) bool {
	var refusal *remoteLaneCancellationRefusedError
	return errors.As(err, &refusal)
}

type agentWireFrame struct {
	Type            string              `json:"type"`
	Version         int                 `json:"version,omitempty"`
	HostID          string              `json:"host_id,omitempty"`
	HostName        string              `json:"host_name,omitempty"`
	TargetHostID    string              `json:"target_host_id,omitempty"`
	RuntimeVersion  string              `json:"runtime_version,omitempty"`
	RuntimeIdentity string              `json:"runtime_identity,omitempty"`
	Generation      uint64              `json:"generation,omitempty"`
	Products        []string            `json:"products,omitempty"`
	Capabilities    []string            `json:"capabilities,omitempty"`
	Hosts           []Host              `json:"hosts,omitempty"`
	Peers           []Peer              `json:"peers,omitempty"`
	SourceID        string              `json:"source_id,omitempty"`
	TargetID        string              `json:"target_id,omitempty"`
	RequestID       string              `json:"request_id,omitempty"`
	Product         string              `json:"product,omitempty"`
	Args            []string            `json:"args,omitempty"`
	Input           []byte              `json:"input,omitempty"`
	Data            []byte              `json:"data,omitempty"`
	ExitCode        int                 `json:"exit_code,omitempty"`
	Frame           json.RawMessage     `json:"frame,omitempty"`
	Parent          *ParentContext      `json:"parent_context,omitempty"`
	RemoteLane      *RemoteLaneEnvelope `json:"remote_lane,omitempty"`
	RemoteAccepted  *RemoteLaneAccepted `json:"remote_accepted,omitempty"`
	RemoteResult    *RemoteLaneResult   `json:"remote_result,omitempty"`
	RemoteArchive   *RemoteLaneArchive  `json:"remote_archive,omitempty"`
	RemoteArchived  *RemoteLaneArchived `json:"remote_archived,omitempty"`
	Error           string              `json:"error,omitempty"`
}

type agentWireConn struct {
	conn         net.Conn
	mu           sync.Mutex
	writeTimeout time.Duration
}

// NewHostAgent validates and constructs a single embedded connection loop.
//
//nolint:gocyclo // Closed connection defaults and identity validation are one constructor gate.
func NewHostAgent(options HostAgentOptions) (*HostAgent, error) {
	options.HubAddress = strings.TrimSpace(options.HubAddress)
	if options.HubAddress == "" {
		return nil, errors.New("embedded federation agent requires a hub address")
	}
	if options.Advertisement.HostID != "" {
		if err := validateHostAdvertisement(options.Advertisement); err != nil {
			return nil, fmt.Errorf("validate embedded host advertisement: %w", err)
		}
		options.HostID, options.HostName = options.Advertisement.HostID, options.Advertisement.HostName
		options.Capabilities = append([]string(nil), options.Advertisement.Capabilities...)
	}
	if !validHostID(options.HostID) {
		return nil, errors.New("embedded federation agent requires a simple canonical host id")
	}
	if strings.TrimSpace(options.HostName) == "" || options.HostName != strings.TrimSpace(options.HostName) {
		return nil, errors.New("embedded federation agent requires a canonical host name")
	}
	options.Capabilities = NormalizeCapabilities(options.Capabilities)
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.DialTimeout <= 0 {
		options.DialTimeout = defaultAgentDialTimeout
	}
	if options.HandshakeTimeout <= 0 {
		options.HandshakeTimeout = defaultAgentHandshakeTimeout
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = defaultAgentWriteTimeout
	}
	if options.DeliveryTimeout <= 0 {
		options.DeliveryTimeout = defaultAgentDeliveryTimeout
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = defaultAgentHeartbeatInterval
	}
	if options.HeartbeatTimeout <= 0 {
		options.HeartbeatTimeout = defaultAgentHeartbeatTimeout
	}
	if options.InitialBackoff <= 0 {
		options.InitialBackoff = defaultAgentInitialBackoff
	}
	if options.MaximumBackoff <= 0 {
		options.MaximumBackoff = defaultAgentMaximumBackoff
	}
	if options.MaximumBackoff < options.InitialBackoff {
		return nil, errors.New("embedded federation maximum backoff is below its initial backoff")
	}
	if options.DialContext == nil {
		dialer := &net.Dialer{Timeout: options.DialTimeout}
		options.DialContext = dialer.DialContext
	}
	return &HostAgent{
		options: options,
		status: AgentStatus{
			HostID: options.HostID, HostName: options.HostName, HubAddress: options.HubAddress,
			State: AgentConnecting, ProtocolVersion: ProtocolVersion,
			Capabilities: append([]string(nil), options.Capabilities...),
		},
		snapshotChanged: make(chan struct{}, 1), pendingDelivery: make(map[string]chan error),
		pendingLane:    make(map[string]chan remoteLaneAdmission),
		pendingCancel:  make(map[string]chan error),
		pendingArchive: make(map[string]chan remoteLaneArchiveDecision),
		pendingResult:  make(map[string]chan error),
	}, nil
}

// Run maintains the one configured connection until ctx is canceled. Hub
// outages are reported through status and retried; they do not terminate the
// host daemon or alter local routing authority.
func (agent *HostAgent) Run(ctx context.Context) error {
	agent.mu.Lock()
	if agent.running {
		agent.mu.Unlock()
		return errors.New("embedded federation agent is already running")
	}
	agent.running = true
	agent.mu.Unlock()
	defer func() {
		agent.disconnect(nil)
		agent.mu.Lock()
		agent.running = false
		agent.mu.Unlock()
	}()

	backoff := agent.options.InitialBackoff
	for ctx.Err() == nil {
		agent.transition(AgentConnecting, "")
		before := agent.Status().ConnectionGeneration
		err := agent.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		state, code := AgentBackoff, agentErrorCode(err)
		var mismatch *ProtocolMismatchError
		if errors.As(err, &mismatch) {
			state = AgentIncompatible
		}
		agent.transition(state, code)
		if agent.Status().ConnectionGeneration != before {
			backoff = agent.options.InitialBackoff
		}
		if err := waitAgentBackoff(ctx, backoff); err != nil {
			return nil
		}
		if backoff < agent.options.MaximumBackoff {
			backoff *= 2
			if backoff > agent.options.MaximumBackoff {
				backoff = agent.options.MaximumBackoff
			}
		}
	}
	return nil
}

// NotifySnapshot requests publication of the latest daemon-owned peer view.
// Repeated notices coalesce and cannot create a second connection authority.
func (agent *HostAgent) NotifySnapshot() {
	select {
	case agent.snapshotChanged <- struct{}{}:
	default:
	}
}

// Status returns an independent metadata-only connection snapshot.
func (agent *HostAgent) Status() AgentStatus {
	agent.mu.RLock()
	defer agent.mu.RUnlock()
	status := agent.status
	status.Capabilities = append([]string(nil), status.Capabilities...)
	return status
}

// PublishRemoteLaneNotice sends one content-free terminal pointer over the
// current authoritative connection. A disconnected host fails explicitly and
// never falls back to local delivery.
func (agent *HostAgent) PublishRemoteLaneNotice(ctx context.Context, notice RemoteLaneNotice, source Peer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	content, err := json.Marshal(notice)
	if err != nil {
		return fmt.Errorf("encode remote lane notice: %w", err)
	}
	if source.ID == "" || source.SessionID != notice.LaneSessionID || source.HostID != agent.options.HostID {
		return errors.New("remote lane notice has an invalid source")
	}
	frame, err := json.Marshal(AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: notice.NoticeID,
		SourceSessionID: source.SessionID, Source: &source, Content: string(content),
	})
	if err != nil {
		return fmt.Errorf("encode remote lane notice frame: %w", err)
	}
	agent.mu.RLock()
	wire := agent.wire
	hostID := agent.options.HostID
	agent.mu.RUnlock()
	if wire == nil {
		return errors.New("federation hub is disconnected")
	}
	result := make(chan error, 1)
	agent.pendingMu.Lock()
	if _, duplicate := agent.pendingDelivery[notice.NoticeID]; duplicate {
		agent.pendingMu.Unlock()
		return errors.New("remote lane notice is already pending")
	}
	agent.pendingDelivery[notice.NoticeID] = result
	agent.pendingMu.Unlock()
	if err := wire.send(agentWireFrame{
		Type: "terminal_notice_deliver", RequestID: notice.NoticeID,
		SourceID: hostID + "/" + notice.LaneSessionID,
		TargetID: notice.TargetHostID + "/" + notice.TargetSessionID,
		Frame:    frame,
	}); err != nil {
		agent.removePendingDelivery(notice.NoticeID)
		return err
	}
	timer := time.NewTimer(agent.options.DeliveryTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		agent.removePendingDelivery(notice.NoticeID)
		return ctx.Err()
	case <-timer.C:
		agent.removePendingDelivery(notice.NoticeID)
		return errors.New("remote lane notice acknowledgement timed out")
	case err := <-result:
		return err
	}
}

// SendRemoteLane emits one typed request and waits only for destination
// durable acceptance. A disconnected or pre-acceptance failure is explicit;
// accepted work remains owned by durable daemon state across reconnects.
func (agent *HostAgent) SendRemoteLane(ctx context.Context, envelope RemoteLaneEnvelope) (RemoteLaneAccepted, error) {
	if envelope.RequestID == "" || envelope.SourceID == "" || envelope.TargetHostID == "" {
		return RemoteLaneAccepted{}, errors.New("remote lane source identity is incomplete")
	}
	agent.mu.RLock()
	wire := agent.wire
	agent.mu.RUnlock()
	if wire == nil {
		return RemoteLaneAccepted{}, errors.New("federation hub is disconnected")
	}
	result := make(chan remoteLaneAdmission, 1)
	agent.pendingMu.Lock()
	if _, duplicate := agent.pendingLane[envelope.RequestID]; duplicate {
		agent.pendingMu.Unlock()
		return RemoteLaneAccepted{}, errors.New("remote lane request is already pending")
	}
	agent.pendingLane[envelope.RequestID] = result
	agent.pendingMu.Unlock()
	if err := wire.send(agentWireFrame{
		Type: "lane_exec", RequestID: envelope.RequestID, SourceID: envelope.SourceID,
		TargetHostID: envelope.TargetHostID, RemoteLane: &envelope,
	}); err != nil {
		agent.removePendingLane(envelope.RequestID)
		return RemoteLaneAccepted{}, err
	}
	timer := time.NewTimer(agent.options.DeliveryTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		agent.removePendingLane(envelope.RequestID)
		return RemoteLaneAccepted{}, ctx.Err()
	case <-timer.C:
		agent.removePendingLane(envelope.RequestID)
		return RemoteLaneAccepted{}, errors.New("remote lane acceptance timed out")
	case admission := <-result:
		return admission.accepted, admission.err
	}
}

// CancelRemoteLane requests cancellation of one already accepted request.
// Durable terminal state still arrives through the content-free notice path.
func (agent *HostAgent) CancelRemoteLane(ctx context.Context, requestID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	agent.mu.RLock()
	wire := agent.wire
	agent.mu.RUnlock()
	if wire == nil {
		return errors.New("federation hub is disconnected")
	}
	result := make(chan error, 1)
	agent.pendingMu.Lock()
	if _, duplicate := agent.pendingCancel[requestID]; duplicate {
		agent.pendingMu.Unlock()
		return errors.New("remote lane cancellation is already pending")
	}
	agent.pendingCancel[requestID] = result
	agent.pendingMu.Unlock()
	if err := wire.send(agentWireFrame{Type: "lane_cancel", RequestID: requestID}); err != nil {
		agent.removePendingCancel(requestID)
		return err
	}
	timer := time.NewTimer(agent.options.DeliveryTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		agent.removePendingCancel(requestID)
		return ctx.Err()
	case <-timer.C:
		agent.removePendingCancel(requestID)
		return errors.New("remote lane cancellation decision timed out")
	case err := <-result:
		return err
	}
}

// ArchiveRemoteLane requests destination-owned native archive and waits for
// one exact acknowledgement. Loss of the acknowledgement is not reported as
// success; retry is safe because destination archive is idempotent.
func (agent *HostAgent) ArchiveRemoteLane(ctx context.Context, request RemoteLaneArchive) (RemoteLaneArchived, error) {
	if request.RequestID == "" || request.RemoteRequestID == "" || request.SourceID == "" ||
		request.TargetHostID == "" || request.Product == "" || request.LaneSessionID == "" {
		return RemoteLaneArchived{}, errors.New("remote lane archive identity is incomplete")
	}
	agent.mu.RLock()
	wire := agent.wire
	agent.mu.RUnlock()
	if wire == nil {
		return RemoteLaneArchived{}, errors.New("federation hub is disconnected")
	}
	pending := make(chan remoteLaneArchiveDecision, 1)
	agent.pendingMu.Lock()
	if _, duplicate := agent.pendingArchive[request.RequestID]; duplicate {
		agent.pendingMu.Unlock()
		return RemoteLaneArchived{}, errors.New("remote lane archive is already pending")
	}
	agent.pendingArchive[request.RequestID] = pending
	agent.pendingMu.Unlock()
	if err := wire.send(agentWireFrame{
		Type: "lane_archive", RequestID: request.RequestID, SourceID: request.SourceID,
		TargetHostID: request.TargetHostID, RemoteArchive: &request,
	}); err != nil {
		agent.removePendingArchive(request.RequestID)
		return RemoteLaneArchived{}, err
	}
	timer := time.NewTimer(agent.options.DeliveryTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		agent.removePendingArchive(request.RequestID)
		return RemoteLaneArchived{}, ctx.Err()
	case <-timer.C:
		agent.removePendingArchive(request.RequestID)
		return RemoteLaneArchived{}, errors.New("remote lane archive acknowledgement timed out")
	case decision := <-pending:
		return decision.archived, decision.err
	}
}

// PublishRemoteLaneResult sends bounded destination result evidence and waits
// for the exact source daemon to durably accept it before a content-free
// terminal notice may be published.
func (agent *HostAgent) PublishRemoteLaneResult(ctx context.Context, result RemoteLaneResult) error {
	if result.RequestID == "" || result.LaneSessionID == "" || result.TurnID == "" || !remoteLaneTerminalOutcome(result.Outcome) {
		return errors.New("remote lane result identity is incomplete")
	}
	if err := ValidateRemoteLaneInput(result.ResultReference); err != nil {
		return err
	}
	agent.mu.RLock()
	wire := agent.wire
	agent.mu.RUnlock()
	if wire == nil {
		return errors.New("federation hub is disconnected")
	}
	pending := make(chan error, 1)
	agent.pendingMu.Lock()
	if _, duplicate := agent.pendingResult[result.RequestID]; duplicate {
		agent.pendingMu.Unlock()
		return errors.New("remote lane result is already pending")
	}
	agent.pendingResult[result.RequestID] = pending
	agent.pendingMu.Unlock()
	if err := wire.send(agentWireFrame{Type: "lane_result", RequestID: result.RequestID, RemoteResult: &result}); err != nil {
		agent.removePendingResult(result.RequestID)
		return err
	}
	timer := time.NewTimer(agent.options.DeliveryTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		agent.removePendingResult(result.RequestID)
		return ctx.Err()
	case <-timer.C:
		agent.removePendingResult(result.RequestID)
		return errors.New("remote lane result acknowledgement timed out")
	case err := <-pending:
		return err
	}
}

//nolint:gocyclo // One connection loop owns handshake, snapshot, heartbeat, and callbacks.
func (agent *HostAgent) runSession(ctx context.Context) error {
	conn, err := agent.options.DialContext(ctx, "tcp", agent.options.HubAddress)
	if err != nil {
		return fmt.Errorf("dial federation hub: %w", err)
	}
	defer func() { _ = conn.Close() }()
	wire := &agentWireConn{conn: conn, writeTimeout: agent.options.WriteTimeout}
	if err := wire.send(agentWireFrame{
		Type: "hello", Version: ProtocolVersion, HostID: agent.options.HostID,
		HostName: agent.options.HostName, Capabilities: agent.options.Capabilities,
		RuntimeVersion:  agent.options.Advertisement.RuntimeVersion,
		RuntimeIdentity: agent.options.Advertisement.RuntimeIdentity,
		Generation:      agent.options.Advertisement.Generation,
		Products:        append([]string(nil), agent.options.Advertisement.Products...),
	}); err != nil {
		return fmt.Errorf("send federation hello: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	if err := conn.SetReadDeadline(time.Now().Add(agent.options.HandshakeTimeout)); err != nil {
		return fmt.Errorf("bound federation handshake: %w", err)
	}
	first, err := scanAgentWireFrame(scanner)
	if err != nil {
		return fmt.Errorf("receive federation hello: %w", err)
	}
	if first.Type == "error" && first.Version != ProtocolVersion {
		return &ProtocolMismatchError{Expected: ProtocolVersion, Received: first.Version}
	}
	if first.Type != "hello_ok" {
		return fmt.Errorf("receive federation hello: first hub frame is %q: %s", first.Type, boundedAgentText(first.Error))
	}
	if first.Version != ProtocolVersion {
		return &ProtocolMismatchError{Expected: ProtocolVersion, Received: first.Version}
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear federation handshake deadline: %w", err)
	}

	agent.connect(wire)
	defer agent.disconnect(wire)
	if err := agent.publishSnapshot(ctx, wire); err != nil {
		return err
	}

	readErr := make(chan error, 1)
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	go func() {
		for {
			message, scanErr := scanAgentWireFrame(scanner)
			if scanErr != nil {
				readErr <- scanErr
				return
			}
			lastActivity.Store(time.Now().UnixNano())
			if handleErr := agent.handleFrame(ctx, wire, message); handleErr != nil {
				readErr <- handleErr
				return
			}
		}
	}()
	pingTicker := time.NewTicker(agent.options.HeartbeatInterval)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case <-agent.snapshotChanged:
			if err := agent.publishSnapshot(ctx, wire); err != nil {
				return err
			}
		case <-pingTicker.C:
			last := time.Unix(0, lastActivity.Load())
			if time.Since(last) > agent.options.HeartbeatTimeout {
				return errors.New("federation hub heartbeat timed out")
			}
			if err := wire.send(agentWireFrame{Type: "ping"}); err != nil {
				return fmt.Errorf("send federation heartbeat: %w", err)
			}
		}
	}
}

func (agent *HostAgent) publishSnapshot(ctx context.Context, wire *agentWireConn) error {
	var peers []Peer
	if callback := agent.options.Callbacks.Snapshot; callback != nil {
		var err error
		peers, err = callback(ctx)
		if err != nil {
			return fmt.Errorf("project federation snapshot: %w", err)
		}
	}
	if err := wire.send(agentWireFrame{Type: "snapshot", Peers: clonePeers(peers)}); err != nil {
		return fmt.Errorf("publish federation snapshot: %w", err)
	}
	return nil
}

//nolint:gocyclo // Every accepted wire family maps to one typed callback or bounded response.
func (agent *HostAgent) handleFrame(ctx context.Context, wire *agentWireConn, message agentWireFrame) error {
	switch message.Type {
	case "pong":
		return nil
	case "roster":
		if callback := agent.options.Callbacks.Roster; callback != nil {
			if err := callback(ctx, Roster{Hosts: cloneHosts(message.Hosts), Peers: clonePeers(message.Peers)}); err != nil {
				return fmt.Errorf("apply federation roster: %w", err)
			}
		}
		agent.mu.Lock()
		agent.status.RemoteRosterGeneration++
		status := cloneAgentStatus(agent.status)
		agent.mu.Unlock()
		agent.notifyState(status)
		return nil
	case "deliver":
		return errors.New("legacy flat delivery is not supported by protocol v3")
	case "group_deliver", "terminal_notice_deliver":
		delivery := RoutedDelivery{
			Type: message.Type, RequestID: message.RequestID, SourceID: message.SourceID,
			TargetID: message.TargetID, Frame: append(json.RawMessage(nil), message.Frame...),
		}
		callback := agent.options.Callbacks.GroupDelivery
		if message.Type == "terminal_notice_deliver" {
			callback = agent.options.Callbacks.TerminalNotice
		}
		var deliveryErr error
		if callback == nil {
			deliveryErr = errors.New("federated delivery is unavailable")
		} else {
			deliveryErr = callback(ctx, delivery)
		}
		response := agentWireFrame{
			Type: "delivery_ack", RequestID: message.RequestID,
			SourceID: message.SourceID, TargetID: message.TargetID,
		}
		if deliveryErr != nil {
			response.Type, response.Error = "delivery_error", boundedAgentError(deliveryErr)
		}
		return wire.send(response)
	case "delivery_ack", "delivery_error":
		agent.completePendingDelivery(message)
		if callback := agent.options.Callbacks.DeliveryResult; callback != nil {
			return callback(ctx, DeliveryOutcome{
				RequestID: message.RequestID, SourceID: message.SourceID,
				TargetID: message.TargetID, Error: message.Error,
			})
		}
		return nil
	case "lane_exec":
		if message.RemoteLane != nil {
			callback := agent.options.Callbacks.RemoteLane
			if callback == nil {
				return wire.send(agentWireFrame{Type: "lane_error", RequestID: message.RequestID, Error: "federated lane execution is unavailable"})
			}
			envelope := cloneRemoteLaneEnvelope(*message.RemoteLane)
			if envelope.RequestID == "" {
				envelope.RequestID = message.RequestID
			}
			accepted, err := callback(ctx, envelope)
			if err != nil {
				return wire.send(agentWireFrame{Type: "lane_error", RequestID: envelope.RequestID, Error: boundedAgentError(err)})
			}
			return wire.send(agentWireFrame{Type: "lane_accepted", RequestID: envelope.RequestID, RemoteAccepted: &accepted})
		}
		callback := agent.options.Callbacks.LaneRequest
		if callback == nil {
			return wire.send(agentWireFrame{Type: "lane_error", RequestID: message.RequestID, Error: "federated lane execution is unavailable"})
		}
		err := callback(ctx, RemoteLaneWireRequest{
			RequestID: message.RequestID, SourceID: message.SourceID, TargetHostID: message.TargetHostID,
			Product: message.Product, Args: append([]string(nil), message.Args...),
			Input: append([]byte(nil), message.Input...), Parent: cloneParentContext(message.Parent),
		})
		if err != nil {
			return wire.send(agentWireFrame{Type: "lane_error", RequestID: message.RequestID, Error: boundedAgentError(err)})
		}
		return nil
	case "lane_cancel":
		if callback := agent.options.Callbacks.LaneCancel; callback != nil {
			if err := callback(ctx, RemoteLaneCancellation{RequestID: message.RequestID}); err != nil {
				return wire.send(agentWireFrame{Type: "lane_cancel_refused", RequestID: message.RequestID, Error: boundedAgentError(err)})
			}
			return wire.send(agentWireFrame{Type: "lane_cancelled", RequestID: message.RequestID})
		}
		return wire.send(agentWireFrame{Type: "lane_cancel_refused", RequestID: message.RequestID, Error: "remote lane cancellation is unavailable"})
	case "lane_cancelled", "lane_cancel_refused":
		agent.completePendingCancel(message)
		return nil
	case "lane_archive":
		if callback := agent.options.Callbacks.LaneArchive; callback != nil && message.RemoteArchive != nil {
			archived, err := callback(ctx, *message.RemoteArchive)
			if err != nil {
				return wire.send(agentWireFrame{Type: "lane_archive_refused", RequestID: message.RequestID, Error: boundedAgentError(err)})
			}
			return wire.send(agentWireFrame{Type: "lane_archived", RequestID: message.RequestID, RemoteArchived: &archived})
		}
		return wire.send(agentWireFrame{Type: "lane_archive_refused", RequestID: message.RequestID, Error: "remote lane archive is unavailable"})
	case "lane_archived", "lane_archive_refused":
		agent.completePendingArchive(message)
		return nil
	case "lane_result_ack", "lane_result_refused":
		agent.completePendingResult(message)
		return nil
	case "lane_result":
		callback := agent.options.Callbacks.LaneResponse
		if callback == nil || message.RemoteResult == nil {
			return wire.send(agentWireFrame{Type: "lane_result_refused", RequestID: message.RequestID, Error: "remote lane result authority is unavailable"})
		}
		err := callback(ctx, RemoteLaneResponse{Type: message.Type, RequestID: message.RequestID, Result: message.RemoteResult})
		if err != nil {
			return wire.send(agentWireFrame{Type: "lane_result_refused", RequestID: message.RequestID, Error: boundedAgentError(err)})
		}
		return wire.send(agentWireFrame{Type: "lane_result_ack", RequestID: message.RequestID})
	case "lane_accepted":
		agent.completePendingLane(message)
		return nil
	case "lane_stdout", "lane_stderr", "lane_exit", "lane_error":
		if message.Type == "lane_error" && agent.completePendingLane(message) {
			return nil
		}
		if callback := agent.options.Callbacks.LaneResponse; callback != nil {
			return callback(ctx, RemoteLaneResponse{
				Type: message.Type, RequestID: message.RequestID, Data: append([]byte(nil), message.Data...),
				ExitCode: message.ExitCode, Error: message.Error,
			})
		}
		return nil
	case "error":
		return fmt.Errorf("federation hub rejected connection: %s", boundedAgentText(message.Error))
	case "hello_ok", "probe", "probe_ok", "snapshot", "ping":
		return fmt.Errorf("out-of-sequence federation frame %q", message.Type)
	default:
		return fmt.Errorf("unsupported federation frame %q", message.Type)
	}
}

func (agent *HostAgent) connect(wire *agentWireConn) {
	agent.mu.Lock()
	agent.wire = wire
	agent.status.State = AgentConnected
	agent.status.ConnectionGeneration++
	agent.status.LastConnectedAt = agent.options.Now().UnixMilli()
	agent.status.LastErrorCode = ""
	status := cloneAgentStatus(agent.status)
	agent.mu.Unlock()
	agent.notifyState(status)
}

func (agent *HostAgent) disconnect(expected *agentWireConn) {
	agent.mu.Lock()
	if expected == nil || agent.wire == expected {
		agent.wire = nil
	}
	agent.mu.Unlock()
	agent.failPendingDeliveries()
}

func (agent *HostAgent) completePendingDelivery(message agentWireFrame) {
	agent.pendingMu.Lock()
	pending := agent.pendingDelivery[message.RequestID]
	delete(agent.pendingDelivery, message.RequestID)
	agent.pendingMu.Unlock()
	if pending == nil {
		return
	}
	if message.Type == "delivery_error" {
		pending <- errors.New(boundedAgentText(message.Error))
	} else {
		pending <- nil
	}
}

func (agent *HostAgent) removePendingDelivery(requestID string) {
	agent.pendingMu.Lock()
	delete(agent.pendingDelivery, requestID)
	agent.pendingMu.Unlock()
}

func (agent *HostAgent) failPendingDeliveries() {
	agent.pendingMu.Lock()
	pending := agent.pendingDelivery
	agent.pendingDelivery = make(map[string]chan error)
	agent.pendingMu.Unlock()
	for _, result := range pending {
		result <- errors.New("federation hub is disconnected")
	}
	agent.pendingMu.Lock()
	lanes := agent.pendingLane
	agent.pendingLane = make(map[string]chan remoteLaneAdmission)
	agent.pendingMu.Unlock()
	for _, result := range lanes {
		result <- remoteLaneAdmission{err: errors.New("federation hub is disconnected before remote lane acceptance")}
	}
	agent.pendingMu.Lock()
	cancellations := agent.pendingCancel
	agent.pendingCancel = make(map[string]chan error)
	agent.pendingMu.Unlock()
	for _, result := range cancellations {
		result <- errors.New("federation hub disconnected before the remote lane cancellation decision")
	}
	agent.pendingMu.Lock()
	results := agent.pendingResult
	agent.pendingResult = make(map[string]chan error)
	agent.pendingMu.Unlock()
	for _, result := range results {
		result <- errors.New("federation hub disconnected before the remote lane result acknowledgement")
	}
	agent.pendingMu.Lock()
	archives := agent.pendingArchive
	agent.pendingArchive = make(map[string]chan remoteLaneArchiveDecision)
	agent.pendingMu.Unlock()
	for _, result := range archives {
		result <- remoteLaneArchiveDecision{err: errors.New("federation hub disconnected before the remote lane archive acknowledgement")}
	}
}

func (agent *HostAgent) completePendingCancel(message agentWireFrame) {
	agent.pendingMu.Lock()
	pending := agent.pendingCancel[message.RequestID]
	delete(agent.pendingCancel, message.RequestID)
	agent.pendingMu.Unlock()
	if pending == nil {
		return
	}
	if message.Type == "lane_cancel_refused" {
		pending <- &remoteLaneCancellationRefusedError{detail: boundedAgentText(message.Error)}
		return
	}
	pending <- nil
}

func (agent *HostAgent) removePendingCancel(requestID string) {
	agent.pendingMu.Lock()
	delete(agent.pendingCancel, requestID)
	agent.pendingMu.Unlock()
}

func (agent *HostAgent) completePendingArchive(message agentWireFrame) {
	agent.pendingMu.Lock()
	pending := agent.pendingArchive[message.RequestID]
	delete(agent.pendingArchive, message.RequestID)
	agent.pendingMu.Unlock()
	if pending == nil {
		return
	}
	if message.Type == "lane_archive_refused" {
		pending <- remoteLaneArchiveDecision{err: fmt.Errorf("remote lane archive was refused: %s", boundedAgentText(message.Error))}
		return
	}
	if message.RemoteArchived == nil || message.RemoteArchived.RequestID != message.RequestID {
		pending <- remoteLaneArchiveDecision{err: errors.New("remote lane archive acknowledgement is incomplete")}
		return
	}
	pending <- remoteLaneArchiveDecision{archived: *message.RemoteArchived}
}

func (agent *HostAgent) removePendingArchive(requestID string) {
	agent.pendingMu.Lock()
	delete(agent.pendingArchive, requestID)
	agent.pendingMu.Unlock()
}

func (agent *HostAgent) completePendingResult(message agentWireFrame) {
	agent.pendingMu.Lock()
	pending := agent.pendingResult[message.RequestID]
	delete(agent.pendingResult, message.RequestID)
	agent.pendingMu.Unlock()
	if pending == nil {
		return
	}
	if message.Type == "lane_result_refused" {
		pending <- fmt.Errorf("remote lane result was refused: %s", boundedAgentText(message.Error))
		return
	}
	pending <- nil
}

func (agent *HostAgent) removePendingResult(requestID string) {
	agent.pendingMu.Lock()
	delete(agent.pendingResult, requestID)
	agent.pendingMu.Unlock()
}

func (agent *HostAgent) completePendingLane(message agentWireFrame) bool {
	agent.pendingMu.Lock()
	pending := agent.pendingLane[message.RequestID]
	if pending != nil {
		delete(agent.pendingLane, message.RequestID)
	}
	agent.pendingMu.Unlock()
	if pending == nil {
		return false
	}
	switch {
	case message.Type == "lane_error":
		pending <- remoteLaneAdmission{err: errors.New(boundedAgentText(message.Error))}
	case message.RemoteAccepted == nil:
		pending <- remoteLaneAdmission{err: errors.New("remote lane acceptance evidence is missing")}
	default:
		pending <- remoteLaneAdmission{accepted: *message.RemoteAccepted}
	}
	return true
}

func (agent *HostAgent) removePendingLane(requestID string) {
	agent.pendingMu.Lock()
	delete(agent.pendingLane, requestID)
	agent.pendingMu.Unlock()
}

func (agent *HostAgent) transition(state AgentConnectionState, errorCode string) {
	agent.mu.Lock()
	agent.status.State = state
	agent.status.LastErrorCode = errorCode
	status := cloneAgentStatus(agent.status)
	agent.mu.Unlock()
	agent.notifyState(status)
}

func (agent *HostAgent) notifyState(status AgentStatus) {
	if callback := agent.options.Callbacks.StateChanged; callback != nil {
		callback(status)
	}
}

func (wire *agentWireConn) send(message agentWireFrame) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(body) > MaxFrameBytes {
		return fmt.Errorf("federation frame exceeds %d bytes", MaxFrameBytes)
	}
	body = append(body, '\n')
	wire.mu.Lock()
	defer wire.mu.Unlock()
	if err := wire.conn.SetWriteDeadline(time.Now().Add(wire.writeTimeout)); err != nil {
		return err
	}
	_, err = wire.conn.Write(body)
	return err
}

func scanAgentWireFrame(scanner *bufio.Scanner) (agentWireFrame, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return agentWireFrame{}, err
		}
		return agentWireFrame{}, io.EOF
	}
	var message agentWireFrame
	if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
		return agentWireFrame{}, fmt.Errorf("decode federation frame: %w", err)
	}
	return message, nil
}

func validHostID(value string) bool {
	return validSimpleID(value)
}

func waitAgentBackoff(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func agentErrorCode(err error) string {
	if err == nil {
		return "connection_closed"
	}
	var mismatch *ProtocolMismatchError
	if errors.As(err, &mismatch) {
		return "protocol_mismatch"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "network_timeout"
	}
	if errors.Is(err, io.EOF) {
		return "connection_closed"
	}
	return "connection_failed"
}

func boundedAgentError(err error) string { return boundedAgentText(err.Error()) }

func boundedAgentText(value string) string {
	const maximum = 512
	value = strings.TrimSpace(value)
	if len(value) > maximum {
		return value[:maximum]
	}
	return value
}

func cloneAgentStatus(status AgentStatus) AgentStatus {
	status.Capabilities = append([]string(nil), status.Capabilities...)
	return status
}

func cloneHosts(hosts []Host) []Host {
	result := append([]Host(nil), hosts...)
	for index := range result {
		result[index].Capabilities = append([]string(nil), result[index].Capabilities...)
	}
	return result
}

func clonePeers(peers []Peer) []Peer {
	result := append([]Peer(nil), peers...)
	for index := range result {
		result[index].Groups = append([]string(nil), result[index].Groups...)
	}
	return result
}

func cloneParentContext(parent *ParentContext) *ParentContext {
	if parent == nil {
		return nil
	}
	clone := *parent
	clone.Groups = append([]string(nil), parent.Groups...)
	return &clone
}

func cloneRemoteLaneEnvelope(envelope RemoteLaneEnvelope) RemoteLaneEnvelope {
	envelope.Parent.Groups = append([]string(nil), envelope.Parent.Groups...)
	envelope.Groups = append([]string(nil), envelope.Groups...)
	envelope.InputReference = cloneRemoteLaneMap(envelope.InputReference)
	return envelope
}
