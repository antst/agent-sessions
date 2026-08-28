package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sharedfederation "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/statestore"
)

type federationComponentOptions struct {
	configuration   DaemonConfig
	generation      uint64
	runtimeVersion  string
	runtimeIdentity string
	attachments     *AttachmentRegistry
	deliveries      *DeliveryEngine
	lanes           *LaneEngine
	laneAdapters    map[string]LaneAdapter
	dialContext     sharedfederation.DialContextFunc
	observe         func(FederationStateRecord)
	now             func() time.Time
	state           *StateStore

	// The injectable session boundary keeps connection/retry tests independent
	// of sockets while production uses configuration and HostAgent below.
	HostID                  string
	HostName                string
	HubAddress              string
	RuntimeVersion          string
	RuntimeIdentity         string
	Generation              uint64
	ReadyProducts           func(context.Context) []string
	RunSession              func(context.Context, sharedfederation.HostAdvertisement, func(string) error) error
	WaitRetry               func(context.Context, time.Duration) error
	PublishState            func(context.Context, FederationStateRecord) error
	SendRemote              func(context.Context, federationDispatchRequest) error
	DispatchLocal           func(context.Context, sharedfederation.AgentFrame) error
	PublishRemoteLaneResult func(context.Context, sharedfederation.RemoteLaneResult) error
	PublishRemoteLaneNotice func(context.Context, sharedfederation.RemoteLaneNotice) error
}

// federationComponent embeds the host side of federation in the daemon
// generation. It owns no listener, child process, shim, or product lifetime.
type federationComponent struct {
	mu      sync.RWMutex
	client  *sharedfederation.HostAgent
	cancel  context.CancelFunc
	done    chan error
	started bool
	status  FederationStateRecord
	observe func(FederationStateRecord)
	now     func() time.Time
	state   *StateStore

	persistMu                sync.Mutex
	stateRevision            statestore.Revision
	persistedRecordRevision  uint64
	connectionGenerationBase uint64

	advertisement           sharedfederation.HostAdvertisement
	readyProducts           func(context.Context) []string
	runSession              func(context.Context, sharedfederation.HostAdvertisement, func(string) error) error
	waitRetry               func(context.Context, time.Duration) error
	publishState            func(context.Context, FederationStateRecord) error
	sendRemote              func(context.Context, federationDispatchRequest) error
	remoteLanes             *sharedfederation.RemoteLaneDispatcher
	deliveries              *DeliveryEngine
	lanes                   *LaneEngine
	remoteRequests          map[string]string
	recoveredSourceRequests map[string]struct{}
	recoveredTerminalOutbox map[string]struct{}
	reconcilingRemote       bool
	reconciledGeneration    uint64
	lifetimeContext         context.Context
	roster                  sharedfederation.Roster
}

type federationDispatchRequest struct {
	RequestID string
	Operation string
	Frame     sharedfederation.AgentFrame
}

type federationDispatchResult struct{ Accepted bool }

type federationDispatchError struct {
	hubAddress string
	cause      error
}

func (failure *federationDispatchError) Error() string {
	return fmt.Sprintf("federation hub %s refused work before acceptance: %v", failure.hubAddress, failure.cause)
}

func (failure *federationDispatchError) Unwrap() error { return failure.cause }

// Accepted reports that transport failed before durable remote acceptance.
func (failure *federationDispatchError) Accepted() bool { return false }

// Retryable reports that a new request may retry after connectivity recovers.
func (failure *federationDispatchError) Retryable() bool { return true }

//nolint:gocyclo // One constructor validates and wires the closed federation authority graph.
func newFederationComponent(options federationComponentOptions) (*federationComponent, error) {
	if options.configuration.HostID == "" && options.HostID != "" {
		options.configuration = DaemonConfig{
			HostID: options.HostID, HostName: options.HostName, HubAddress: options.HubAddress,
			RemoteLanesEnabled: true,
		}
		options.generation = options.Generation
		options.runtimeVersion = options.RuntimeVersion
		options.runtimeIdentity = options.RuntimeIdentity
	}
	if options.attachments == nil || options.generation == 0 {
		if options.RunSession == nil || options.generation == 0 {
			return nil, errors.New("embedded federation requires attachment authority and generation")
		}
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.state == nil && options.attachments != nil {
		options.state = options.attachments.state
	}
	products, capabilities := readyFederationProducts(options.configuration.RemoteLanesEnabled, options.laneAdapters)
	if options.ReadyProducts != nil {
		products, capabilities = federationProductsAndCapabilities(options.ReadyProducts(context.Background()))
	}
	component := &federationComponent{
		status: FederationStateRecord{
			RecordHeader: RecordHeader{
				SchemaVersion: HostRuntimeSchemaVersion, Revision: 1,
				Generation: options.generation, CreatedAt: options.now().UnixMilli(), UpdatedAt: options.now().UnixMilli(),
			},
			HostID: options.configuration.HostID, HostName: options.configuration.HostName,
			HubAddress: options.configuration.HubAddress, ProtocolVersion: sharedfederation.ProtocolVersion,
			AdvertisedCapabilities: capabilities, State: string(sharedfederation.AgentDisabled),
			AdvertisedRuntimeVersion: options.runtimeVersion, AdvertisedRuntimeIdentity: options.runtimeIdentity,
			AdvertisedProducts: products,
		},
		observe: options.observe, now: options.now, state: options.state,
		advertisement: sharedfederation.HostAdvertisement{
			HostID: options.configuration.HostID, HostName: options.configuration.HostName,
			ProtocolVersion: sharedfederation.ProtocolVersion, Generation: options.generation,
			RuntimeVersion: options.runtimeVersion, RuntimeIdentity: options.runtimeIdentity,
			Products: products, Capabilities: capabilities,
		},
		readyProducts: options.ReadyProducts, runSession: options.RunSession,
		waitRetry: options.WaitRetry, publishState: options.PublishState, sendRemote: options.SendRemote,
		deliveries: options.deliveries, remoteRequests: make(map[string]string),
		recoveredSourceRequests: make(map[string]struct{}),
		recoveredTerminalOutbox: make(map[string]struct{}),
	}
	if options.lanes != nil {
		for _, turn := range options.lanes.RemoteTurns(true) {
			if len(turn.RemoteEnvelope) != 0 {
				component.recoveredSourceRequests[turn.RemoteRequestID] = struct{}{}
			}
		}
		for _, turn := range options.lanes.RemoteTurns(false) {
			if turn.TerminalOutcome != "" && turn.RemoteNoticeAcknowledgedAt == 0 {
				component.recoveredTerminalOutbox[turn.RemoteRequestID] = struct{}{}
			}
			if turn.TerminalOutcome == "" {
				component.remoteRequests[laneTurnKey(turn.LaneSessionID, turn.TurnID)] = turn.RemoteRequestID
			}
		}
	}
	if options.state != nil {
		prior, revision, err := options.state.ReadFederation(context.Background())
		if err == nil {
			if err := validateFederationStateRecord(prior); err != nil {
				return nil, fmt.Errorf("recover federation state: %w", err)
			}
			if prior.HostID != options.configuration.HostID {
				return nil, errors.New("recovered federation state belongs to another host identity")
			}
			component.stateRevision = revision
			component.persistedRecordRevision = prior.Revision
			component.connectionGenerationBase = prior.ConnectionGeneration
			component.status.Revision = prior.Revision + 1
			component.status.CreatedAt = prior.CreatedAt
			component.status.ConnectionGeneration = prior.ConnectionGeneration
			if prior.HubAddress == options.configuration.HubAddress {
				component.status.RemoteRosterRevision = prior.RemoteRosterRevision
				component.status.LastConnectedAt = prior.LastConnectedAt
				component.status.LastErrorCode = prior.LastErrorCode
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("recover federation state: %w", err)
		}
	}
	if options.lanes != nil {
		publishNotice := options.PublishRemoteLaneNotice
		if publishNotice == nil {
			publishNotice = component.publishRemoteLaneNotice
		}
		publishResult := options.PublishRemoteLaneResult
		if publishResult == nil {
			publishResult = component.publishRemoteLaneOutcome
		}
		dispatcher, err := sharedfederation.NewRemoteLaneDispatcher(sharedfederation.RemoteLaneDispatcherOptions{
			LocalHostID: options.configuration.HostID, Handler: component,
			PublishResult: publishResult,
			PublishNotice: publishNotice,
		})
		if err != nil {
			return nil, err
		}
		component.remoteLanes = dispatcher
		component.lanes = options.lanes
		for _, turn := range options.lanes.RemoteTurns(false) {
			lane, readErr := options.lanes.ReadLane(context.Background(), turn.LaneSessionID)
			if readErr != nil {
				return nil, fmt.Errorf("recover remote lane %s: %w", turn.RemoteRequestID, readErr)
			}
			if err := dispatcher.RestoreAccepted(sharedfederation.RemoteLaneRequest{
				RequestID: turn.RemoteRequestID, TargetHostID: options.configuration.HostID,
				Product: lane.Product, LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID,
				ParentHostID: lane.ParentHostID, ParentSessionID: lane.ParentSessionID,
			}, sharedfederation.RemoteLaneAccepted{
				RequestID: turn.RemoteRequestID, LaneSessionID: lane.LaneSessionID,
				TurnID: turn.TurnID, AcceptedRevision: turn.Revision,
			}, turn.RemoteFingerprint); err != nil {
				return nil, fmt.Errorf("recover remote lane dispatcher %s: %w", turn.RemoteRequestID, err)
			}
		}
	}
	if options.configuration.HubAddress == "" || options.RunSession != nil {
		return component, nil
	}
	client, err := sharedfederation.NewHostAgent(sharedfederation.HostAgentOptions{
		HubAddress: options.configuration.HubAddress,
		HostID:     options.configuration.HostID, HostName: options.configuration.HostName,
		Capabilities: capabilities, Advertisement: component.advertisement, DialContext: options.dialContext,
		Callbacks: sharedfederation.AgentCallbacks{
			Snapshot: func(context.Context) ([]sharedfederation.Peer, error) {
				return federationAttachmentSnapshot(options.attachments, options.configuration.HostName), nil
			},
			Roster:         component.acceptFederationRoster,
			GroupDelivery:  component.acceptFederatedDelivery,
			TerminalNotice: component.acceptRemoteLaneNotice,
			LaneRequest: func(context.Context, sharedfederation.RemoteLaneWireRequest) error {
				return errors.New("federated lane routing is not recovered")
			},
			RemoteLane:   component.DispatchRemoteLane,
			LaneCancel:   component.cancelRemoteLane,
			LaneArchive:  component.archiveRemoteLane,
			LaneResponse: component.acceptRemoteLaneResponse,
			StateChanged: component.updateStatus,
		},
	})
	if err != nil {
		return nil, err
	}
	component.client = client
	component.updateStatus(client.Status())
	return component, nil
}

func (component *federationComponent) acceptFederationRoster(ctx context.Context, roster sharedfederation.Roster) error {
	component.mu.Lock()
	component.roster = sharedfederation.Roster{
		Hosts: append([]sharedfederation.Host(nil), roster.Hosts...),
		Peers: append([]sharedfederation.Peer(nil), roster.Peers...),
	}
	connectionGeneration := uint64(0)
	if component.client != nil {
		connectionGeneration = component.client.Status().ConnectionGeneration
	}
	if connectionGeneration > component.reconciledGeneration && component.lanes != nil {
		for _, turn := range component.lanes.RemoteTurns(true) {
			if len(turn.RemoteEnvelope) != 0 {
				component.recoveredSourceRequests[turn.RemoteRequestID] = struct{}{}
			}
		}
		for _, turn := range component.lanes.RemoteTurns(false) {
			if turn.TerminalOutcome != "" && turn.RemoteNoticeAcknowledgedAt == 0 {
				component.recoveredTerminalOutbox[turn.RemoteRequestID] = struct{}{}
			}
		}
		component.reconciledGeneration = connectionGeneration
	}
	if component.reconcilingRemote ||
		(len(component.recoveredSourceRequests) == 0 && len(component.recoveredTerminalOutbox) == 0) {
		component.mu.Unlock()
		return nil
	}
	component.reconcilingRemote = true
	component.mu.Unlock()
	go component.reconcileRecoveredRemoteSources(ctx)
	return nil
}

func (component *federationComponent) scheduleRemoteReconciliation() {
	component.mu.Lock()
	if component.reconcilingRemote || component.lifetimeContext == nil ||
		(len(component.recoveredSourceRequests) == 0 && len(component.recoveredTerminalOutbox) == 0) {
		component.mu.Unlock()
		return
	}
	component.reconcilingRemote = true
	ctx := component.lifetimeContext
	component.mu.Unlock()
	go component.reconcileRecoveredRemoteSources(ctx)
}

func (component *federationComponent) reconcileRecoveredRemoteSources(ctx context.Context) {
	defer func() {
		component.mu.Lock()
		component.reconcilingRemote = false
		component.mu.Unlock()
	}()
	backoff := 250 * time.Millisecond
	for component.reconcileRecoveredRemoteSourcesOnce(ctx) {
		if waitFederationRetry(ctx, backoff) != nil {
			return
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
}

//nolint:gocyclo // Source replay, cancellation recovery, and terminal outbox retry are one ordered recovery pass.
func (component *federationComponent) reconcileRecoveredRemoteSourcesOnce(ctx context.Context) bool {
	component.mu.RLock()
	client := component.client
	requestIDs := make([]string, 0, len(component.recoveredSourceRequests))
	for requestID := range component.recoveredSourceRequests {
		requestIDs = append(requestIDs, requestID)
	}
	component.mu.RUnlock()
	sort.Strings(requestIDs)
	for _, requestID := range requestIDs {
		if client == nil {
			break
		}
		lane, turn, err := component.lanes.RemoteTurn(requestID)
		if err != nil {
			continue
		}
		envelope, err := remoteLaneEnvelopeFromEvidence(turn.RemoteEnvelope)
		if err != nil || envelope.RequestID != requestID || envelope.TargetHostID != lane.RemoteHostID {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		accepted, sendErr := client.SendRemoteLane(requestCtx, envelope)
		cancel()
		if sendErr != nil || accepted.RequestID != requestID || accepted.LaneSessionID != lane.LaneSessionID || accepted.TurnID != turn.TurnID {
			continue
		}
		_, turn, err = component.lanes.MarkRemoteSourceRunning(ctx, requestID, accepted.AcceptedRevision)
		if err != nil {
			continue
		}
		if turn.TerminalOutcome == "" && turn.RemoteCancellationState == "pending" {
			requestCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
			cancelErr := client.CancelRemoteLane(requestCtx, requestID)
			cancel()
			if cancelErr != nil {
				if sharedfederation.IsRemoteLaneCancellationRefused(cancelErr) {
					_, _ = component.lanes.ResolveRemoteCancellation(ctx, requestID, false, cancelErr.Error())
				}
				continue
			}
			if _, err = component.lanes.ResolveRemoteCancellation(ctx, requestID, true, ""); err != nil {
				continue
			}
		}
		component.mu.Lock()
		delete(component.recoveredSourceRequests, requestID)
		component.mu.Unlock()
	}
	component.mu.RLock()
	outbox := make([]string, 0, len(component.recoveredTerminalOutbox))
	for requestID := range component.recoveredTerminalOutbox {
		outbox = append(outbox, requestID)
	}
	component.mu.RUnlock()
	sort.Strings(outbox)
	for _, requestID := range outbox {
		_, turn, err := component.lanes.RemoteTurn(requestID)
		if err != nil || turn.TerminalOutcome == "" || turn.RemoteNoticeAcknowledgedAt != 0 {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, publishErr := component.PublishRemoteLaneResult(requestCtx, sharedfederation.RemoteLaneResult{
			RequestID: requestID, LaneSessionID: turn.LaneSessionID, TurnID: turn.TurnID,
			Outcome: turn.TerminalOutcome, ResultReference: cloneAttachmentEvidence(turn.ResultReference),
		})
		cancel()
		if publishErr != nil {
			continue
		}
		component.mu.Lock()
		delete(component.recoveredTerminalOutbox, requestID)
		component.mu.Unlock()
	}
	component.mu.RLock()
	pending := len(component.recoveredSourceRequests) != 0 || len(component.recoveredTerminalOutbox) != 0
	component.mu.RUnlock()
	return pending
}

func (component *federationComponent) acceptRemoteLaneNotice(
	ctx context.Context,
	delivery sharedfederation.RoutedDelivery,
) error {
	var frame sharedfederation.AgentFrame
	if err := json.Unmarshal(delivery.Frame, &frame); err != nil {
		return fmt.Errorf("decode remote lane terminal frame: %w", err)
	}
	var notice sharedfederation.RemoteLaneNotice
	if err := json.Unmarshal([]byte(frame.Content), &notice); err != nil {
		return fmt.Errorf("decode remote lane terminal notice: %w", err)
	}
	lane, turn, err := component.lanes.RemoteTurn(notice.RequestID)
	if err != nil {
		return err
	}
	if notice.LaneSessionID != lane.LaneSessionID || notice.TurnID != turn.TurnID ||
		notice.TargetHostID != lane.ParentHostID || notice.TargetSessionID != lane.ParentSessionID ||
		delivery.SourceID != lane.RemoteHostID+"/"+lane.LaneSessionID ||
		delivery.TargetID != lane.ParentHostID+"/"+lane.ParentSessionID ||
		turn.RemoteResultOutcome != notice.Outcome || !validLaneTerminalOutcome(notice.Outcome) {
		return ErrLaneIdempotencyConflict
	}
	resultReference := cloneAttachmentEvidence(turn.RemoteResultReference)
	if resultReference == nil {
		resultReference = make(map[string]any)
	}
	resultReference["remote_notice_id"] = notice.NoticeID
	resultReference["remote_host_id"] = lane.RemoteHostID
	if _, err := component.lanes.Complete(ctx, LaneTerminalRequest{
		LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, Outcome: notice.Outcome,
		NativeTurnIdentity: turn.NativeTurnIdentity,
		ResultReference:    resultReference,
	}); err != nil {
		return err
	}
	return component.acceptFederatedDelivery(ctx, delivery)
}

func (component *federationComponent) acceptFederatedDelivery(
	ctx context.Context,
	delivery sharedfederation.RoutedDelivery,
) error {
	if component.deliveries == nil {
		return errors.New("daemon delivery authority is unavailable")
	}
	_, err := component.deliveries.AcceptFederated(ctx, delivery)
	return err
}

// DispatchRemoteLane applies hub-attested logical lane input directly to the
// daemon lane authority without a watcher, local socket, or child manager.
func (component *federationComponent) DispatchRemoteLane(
	ctx context.Context,
	envelope sharedfederation.RemoteLaneEnvelope,
) (sharedfederation.RemoteLaneAccepted, error) {
	if component.remoteLanes == nil {
		return sharedfederation.RemoteLaneAccepted{}, errors.New("remote lane routing is unavailable")
	}
	accepted, err := component.remoteLanes.Dispatch(ctx, envelope)
	if err != nil {
		return accepted, err
	}
	if turn, terminalErr := component.lanes.terminalTurn(accepted.LaneSessionID, accepted.TurnID); terminalErr == nil {
		_, err = component.PublishRemoteLaneResult(ctx, sharedfederation.RemoteLaneResult{
			RequestID: accepted.RequestID, LaneSessionID: accepted.LaneSessionID, TurnID: accepted.TurnID,
			Outcome: turn.TerminalOutcome, ResultReference: cloneAttachmentEvidence(turn.ResultReference),
		})
	}
	return accepted, err
}

// PublishRemoteLaneResult emits one idempotent content-free parent notice.
func (component *federationComponent) PublishRemoteLaneResult(
	ctx context.Context,
	result sharedfederation.RemoteLaneResult,
) (sharedfederation.RemoteLaneNotice, error) {
	if component.remoteLanes == nil {
		return sharedfederation.RemoteLaneNotice{}, errors.New("remote lane routing is unavailable")
	}
	notice, err := component.remoteLanes.PublishResult(ctx, result)
	if err != nil {
		return notice, err
	}
	if _, err := component.lanes.AcknowledgeRemoteNotice(ctx, result.RequestID); err != nil {
		return notice, err
	}
	component.mu.Lock()
	delete(component.recoveredTerminalOutbox, result.RequestID)
	component.mu.Unlock()
	return notice, nil
}

// StartRemoteLane implements federation.RemoteLaneHandler through the one
// daemon LaneEngine. The remote parent is never inserted into the local
// attachment registry.
func (component *federationComponent) StartRemoteLane(
	ctx context.Context,
	request sharedfederation.RemoteLaneRequest,
) (sharedfederation.RemoteLaneAccepted, error) {
	if component.lanes == nil {
		return sharedfederation.RemoteLaneAccepted{}, errors.New("daemon lane authority is unavailable")
	}
	fingerprint, err := sharedfederation.RemoteLaneRequestFingerprint(request)
	if err != nil {
		return sharedfederation.RemoteLaneAccepted{}, err
	}
	parent := AttachmentRecord{
		RecordHeader: RecordHeader{SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: component.snapshot().Generation},
		AttachmentID: request.ParentInstanceID, SessionID: request.ParentSessionID,
		Kind: sharedfederation.SessionKindInteractive, Product: request.ParentProduct,
		HostID: request.ParentHostID, Groups: append([]string(nil), request.ParentGroups...),
		PermissionMode: request.ParentPermissionMode, State: AttachmentStateAttached,
	}
	key := laneTurnKey(request.LaneSessionID, request.TurnID)
	component.mu.Lock()
	component.remoteRequests[key] = request.RequestID
	component.mu.Unlock()
	normalizedStart, rollback, err := component.normalizeRemoteLaneStart(ctx, request)
	if err != nil {
		component.mu.Lock()
		if component.remoteRequests[key] == request.RequestID {
			delete(component.remoteRequests, key)
		}
		component.mu.Unlock()
		return sharedfederation.RemoteLaneAccepted{}, err
	}
	lane, turn, err := component.lanes.startWithParent(ctx, LaneStartRequest{
		LaneSessionID: request.LaneSessionID, TurnID: request.TurnID,
		SourceAttachmentID: request.ParentInstanceID, Product: request.Product,
		Name: request.Name, Cwd: normalizedStart.Cwd, Groups: append([]string(nil), request.Groups...),
		InheritParentGroups: request.InheritParentGroups, PermissionMode: normalizedStart.PermissionMode,
		InputReference: normalizedStart.InputReference, AllowDuplicateName: request.AllowDuplicateName,
		RemoteRequestID:   request.RequestID,
		RemoteFingerprint: fingerprint,
	}, parent, request.TargetHostID)
	if err != nil && turn.Revision == 0 {
		if rollback != nil {
			err = errors.Join(err, rollback())
		}
		component.mu.Lock()
		if component.remoteRequests[key] == request.RequestID {
			delete(component.remoteRequests, key)
		}
		component.mu.Unlock()
		return sharedfederation.RemoteLaneAccepted{}, err
	}
	return sharedfederation.RemoteLaneAccepted{
		RequestID: request.RequestID, LaneSessionID: lane.LaneSessionID,
		TurnID: turn.TurnID, AcceptedRevision: turn.Revision,
	}, nil
}

type normalizedRemoteLaneStart struct {
	Cwd            string
	PermissionMode string
	InputReference map[string]any
}

// normalizeRemoteLaneStart is deliberately destination-owned. Remote callers
// serialize their requested cwd and canonical arguments; only the target host
// may resolve paths, create worktrees, or select a native product profile.
func (component *federationComponent) normalizeRemoteLaneStart(
	ctx context.Context,
	request sharedfederation.RemoteLaneRequest,
) (normalizedRemoteLaneStart, func() error, error) {
	input := cloneAttachmentEvidence(request.InputReference)
	options := attachmentEvidenceMap(input["options"])
	command, _ := options["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		command = "start"
	}
	arguments, err := remoteLaneStringSlice(options["arguments"])
	if err != nil {
		return normalizedRemoteLaneStart{}, nil, err
	}
	cwd := strings.TrimSpace(request.Cwd)
	if cwd == "" {
		cwd = "."
	}
	if !filepath.IsAbs(cwd) {
		cwd, err = filepath.Abs(cwd)
		if err != nil {
			return normalizedRemoteLaneStart{}, nil, fmt.Errorf("resolve destination lane cwd: %w", err)
		}
	}
	cwd = filepath.Clean(cwd)
	var nativeActor map[string]any
	if existing, readErr := component.lanes.ReadLane(ctx, request.LaneSessionID); readErr == nil {
		cwd = existing.Cwd
		nativeActor = cloneAttachmentEvidence(existing.NativeActor)
	} else if !errors.Is(readErr, ErrLaneNotFound) {
		return normalizedRemoteLaneStart{}, nil, readErr
	}
	normalized, err := component.lanes.normalizeLaneCommand(ctx, LaneCommandNormalizationRequest{
		Product: request.Product, Command: command, Arguments: arguments,
		LaneSessionID: request.LaneSessionID, Name: request.Name, Cwd: cwd,
		PermissionMode: request.PermissionMode, NativeActor: nativeActor,
	})
	if err != nil {
		return normalizedRemoteLaneStart{}, nil, err
	}
	if strings.TrimSpace(normalized.Cwd) != "" {
		cwd = normalized.Cwd
	}
	permission := request.PermissionMode
	if strings.TrimSpace(normalized.PermissionMode) != "" {
		permission = normalized.PermissionMode
	}
	if options == nil {
		options = make(map[string]any)
	}
	options["command"] = command
	options["arguments"] = append([]string(nil), arguments...)
	options["native"] = cloneAttachmentEvidence(normalized.NativeOptions)
	if input == nil {
		input = make(map[string]any)
	}
	input["options"] = options
	return normalizedRemoteLaneStart{
		Cwd: cwd, PermissionMode: permission, InputReference: input,
	}, normalized.Rollback, nil
}

func attachmentEvidenceMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if result, ok := value.(map[string]any); ok {
		return cloneAttachmentEvidence(result)
	}
	return nil
}

func remoteLaneStringSlice(value any) ([]string, error) {
	switch values := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), values...), nil
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, errors.New("remote lane arguments are not strings")
			}
			result = append(result, text)
		}
		return result, nil
	default:
		return nil, errors.New("remote lane arguments are not an array")
	}
}

func (component *federationComponent) cancelRemoteLane(ctx context.Context, cancellation sharedfederation.RemoteLaneCancellation) error {
	component.mu.RLock()
	var laneSessionID, turnID string
	for key, requestID := range component.remoteRequests {
		if requestID == cancellation.RequestID {
			parts := strings.SplitN(key, "\x00", 2)
			if len(parts) == 2 {
				laneSessionID, turnID = parts[0], parts[1]
			}
			break
		}
	}
	component.mu.RUnlock()
	if laneSessionID == "" {
		return ErrLaneNotFound
	}
	_, err := component.lanes.Interrupt(ctx, LaneCollectRequest{LaneSessionID: laneSessionID, TurnID: turnID})
	if err != nil {
		return err
	}
	component.mu.Lock()
	component.recoveredTerminalOutbox[cancellation.RequestID] = struct{}{}
	component.mu.Unlock()
	component.scheduleRemoteReconciliation()
	// Cancellation acceptance is the native interrupt decision, not the
	// availability of the separate durable terminal-result route. The outbox
	// retries that result without turning an accepted interrupt into refusal.
	return nil
}

func (component *federationComponent) archiveRemoteLane(
	ctx context.Context,
	request sharedfederation.RemoteLaneArchive,
) (sharedfederation.RemoteLaneArchived, error) {
	if component.lanes == nil || request.TargetHostID != component.snapshot().HostID {
		return sharedfederation.RemoteLaneArchived{}, ErrLaneIdempotencyConflict
	}
	lane, turn, err := component.lanes.RemoteTurn(request.RemoteRequestID)
	if err != nil || lane.LaneSessionID != request.LaneSessionID || lane.Product != request.Product ||
		lane.RemoteHostID != "" || request.SourceID != lane.ParentHostID+"/"+lane.ParentSessionID ||
		turn.TerminalOutcome == "" {
		return sharedfederation.RemoteLaneArchived{}, ErrLaneIdempotencyConflict
	}
	archived, err := component.lanes.Archive(ctx, LaneArchiveRequest{LaneSessionID: lane.LaneSessionID})
	if err != nil {
		return sharedfederation.RemoteLaneArchived{}, err
	}
	return sharedfederation.RemoteLaneArchived{
		RequestID: request.RequestID, LaneSessionID: archived.LaneSessionID, ArchiveRevision: archived.ArchiveRevision,
	}, nil
}

func (component *federationComponent) acceptRemoteLaneResponse(ctx context.Context, response sharedfederation.RemoteLaneResponse) error {
	if response.Type == "lane_result" {
		if response.Result == nil || response.Result.RequestID != response.RequestID {
			return ErrLaneIdempotencyConflict
		}
		lane, turn, err := component.lanes.RemoteTurn(response.RequestID)
		if err != nil || response.Result.LaneSessionID != lane.LaneSessionID || response.Result.TurnID != turn.TurnID {
			return ErrLaneIdempotencyConflict
		}
		if err := sharedfederation.ValidateRemoteLaneInput(response.Result.ResultReference); err != nil {
			return err
		}
		_, err = component.lanes.StageRemoteResult(
			ctx, response.RequestID, response.Result.Outcome, response.Result.ResultReference,
		)
		return err
	}
	if response.Type != "lane_exit" && response.Type != "lane_error" {
		return nil
	}
	lane, turn, err := component.lanes.RemoteTurn(response.RequestID)
	if err != nil {
		return err
	}
	outcome := LaneDispatchCompleted
	if response.Type == "lane_error" || response.ExitCode != 0 {
		outcome = LaneDispatchFailed
	}
	_, err = component.lanes.Complete(ctx, LaneTerminalRequest{
		LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, Outcome: outcome,
		NativeTurnIdentity: turn.NativeTurnIdentity,
		ResultReference:    map[string]any{"remote_host_id": lane.RemoteHostID, "error_code": response.Type},
	})
	return err
}

func (component *federationComponent) publishRemoteLaneOutcome(
	ctx context.Context,
	result sharedfederation.RemoteLaneResult,
) error {
	component.mu.RLock()
	client := component.client
	component.mu.RUnlock()
	if client == nil {
		return errors.New("federation hub is disconnected")
	}
	return client.PublishRemoteLaneResult(ctx, result)
}

//nolint:gocyclo // The closed remote lane command family shares one canonical parser and durable authority gate.
func (component *federationComponent) executeRemoteLaneCommand(
	ctx context.Context,
	engine *LaneEngine,
	attachments *AttachmentRegistry,
	sourceAttachmentID string,
	request LaneCommandRequest,
) (map[string]any, error) {
	parent, ok := attachments.attachedByID(sourceAttachmentID)
	if !ok {
		return nil, ErrAttachmentNotAttested
	}
	request.Host = strings.TrimSpace(request.Host)
	options, positionals, err := parseLaneCommandOptions(request.Product, request.Command, request.Arguments)
	if err != nil {
		return nil, err
	}
	if options.promptFile != "" {
		return nil, errors.New("--prompt-file is not supported for remote lanes; provide bounded prompt input on stdin")
	}
	command := strings.TrimSpace(request.Command)
	base := map[string]any{"type": "lane." + command, "product": request.Product, "command": command, "host": request.Host}
	switch command {
	case "wait":
		if len(positionals) != 1 {
			return nil, errors.New("lane wait requires one session id or unique visible name")
		}
		lane, err := resolveRemoteLaneCommandSelector(ctx, engine, sourceAttachmentID, request, positionals[0], true, false)
		if err != nil {
			return nil, err
		}
		turn, err := engine.latestLaneTurn(ctx, lane.LaneSessionID, sourceAttachmentID)
		if err != nil {
			return nil, err
		}
		waitCtx, cancel, err := laneCommandWaitContext(ctx, options.timeout)
		if err != nil {
			return nil, err
		}
		defer cancel()
		collection, err := engine.Wait(waitCtx, LaneCollectRequest{
			LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, SourceAttachmentID: sourceAttachmentID,
		})
		if err != nil {
			return nil, err
		}
		base["lane"], base["turn"], base["collection"] = publicLaneRecord(collection.Lane), publicLaneTurn(collection.Turn), publicLaneCollection(collection)
		return base, nil
	case "status":
		if len(positionals) != 1 {
			return nil, errors.New("lane status requires one session id or unique visible name")
		}
		lane, err := resolveRemoteLaneCommandSelector(ctx, engine, sourceAttachmentID, request, positionals[0], true, options.mine)
		if err != nil {
			return nil, err
		}
		base["lane"] = publicLaneRecord(lane)
		if turn, turnErr := engine.latestLaneTurn(ctx, lane.LaneSessionID, sourceAttachmentID); turnErr == nil {
			base["turn"] = publicLaneTurn(turn)
		}
		return base, nil
	case "list":
		if len(positionals) != 0 {
			return nil, errors.New("lane list does not accept a target")
		}
		lanes, err := engine.List(ctx, LaneListRequest{SourceAttachmentID: sourceAttachmentID, All: options.all, Mine: options.mine})
		if err != nil {
			return nil, err
		}
		visible := make([]LaneRecord, 0, len(lanes))
		for _, lane := range lanes {
			if lane.Product == request.Product && lane.RemoteHostID == request.Host {
				visible = append(visible, publicLaneRecord(lane))
			}
		}
		base["lanes"] = visible
		return base, nil
	case "doctor":
		if len(positionals) != 0 {
			return nil, errors.New("lane doctor does not accept a target")
		}
		component.mu.RLock()
		roster, client := component.roster, component.client
		component.mu.RUnlock()
		if client == nil || client.Status().State != sharedfederation.AgentConnected {
			return nil, errors.New("federation hub is disconnected")
		}
		ready, err := remoteLaneDoctorReady(roster, request.Host, request.Product)
		base["ready"], base["authority"] = ready, "remote-daemon"
		if err != nil {
			return base, err
		}
		return base, nil
	case "interrupt":
		if len(positionals) != 1 {
			return nil, errors.New("lane interrupt requires one session id or unique visible name")
		}
		lane, err := resolveRemoteLaneCommandSelector(ctx, engine, sourceAttachmentID, request, positionals[0], false, options.mine)
		if err != nil {
			return nil, err
		}
		turn, err := engine.latestLaneTurn(ctx, lane.LaneSessionID, sourceAttachmentID)
		if err != nil {
			return nil, err
		}
		if lane.RemoteHostID != request.Host || turn.RemoteRequestID == "" {
			return nil, ErrLaneIdempotencyConflict
		}
		turn, err = engine.RequestRemoteCancellation(ctx, turn.RemoteRequestID)
		if err != nil {
			return nil, err
		}
		component.mu.RLock()
		client := component.client
		component.mu.RUnlock()
		if client == nil {
			return nil, errors.New("remote lane cancellation decision is pending until federation reconnects")
		}
		if err := client.CancelRemoteLane(ctx, turn.RemoteRequestID); err != nil {
			if sharedfederation.IsRemoteLaneCancellationRefused(err) {
				resolved, resolveErr := engine.ResolveRemoteCancellation(ctx, turn.RemoteRequestID, false, err.Error())
				base["lane"], base["turn"] = publicLaneRecord(lane), publicLaneTurn(resolved)
				return base, errors.Join(err, resolveErr)
			}
			return nil, fmt.Errorf("remote lane cancellation decision remains pending and will retry after reconnect: %w", err)
		}
		turn, err = engine.ResolveRemoteCancellation(ctx, turn.RemoteRequestID, true, "")
		if err != nil {
			return nil, err
		}
		base["lane"], base["turn"] = publicLaneRecord(lane), publicLaneTurn(turn)
		return base, nil
	case "archive":
		if len(positionals) != 1 {
			return nil, errors.New("lane archive requires one session id or unique visible name")
		}
		lane, err := resolveRemoteLaneCommandSelector(ctx, engine, sourceAttachmentID, request, positionals[0], true, options.mine)
		if err != nil {
			return nil, err
		}
		if lane.State == LaneStateArchived {
			base["lane"] = publicLaneRecord(lane)
			return base, nil
		}
		turn, err := engine.latestLaneTurn(ctx, lane.LaneSessionID, sourceAttachmentID)
		if err != nil || turn.TerminalOutcome == "" || turn.RemoteRequestID == "" {
			return nil, ErrLaneNotTerminal
		}
		archiveID, err := randomControlRequestID()
		if err != nil {
			return nil, err
		}
		component.mu.RLock()
		client := component.client
		component.mu.RUnlock()
		if client == nil || client.Status().State != sharedfederation.AgentConnected {
			return nil, errors.New("federation hub is disconnected before remote lane archive")
		}
		archived, err := client.ArchiveRemoteLane(ctx, sharedfederation.RemoteLaneArchive{
			RequestID: archiveID, RemoteRequestID: turn.RemoteRequestID,
			SourceID: parent.HostID + "/" + parent.SessionID, TargetHostID: request.Host,
			Product: request.Product, LaneSessionID: lane.LaneSessionID,
		})
		if err != nil {
			return nil, fmt.Errorf("remote lane archive is unconfirmed and may be retried safely: %w", err)
		}
		if archived.RequestID != archiveID || archived.LaneSessionID != lane.LaneSessionID || archived.ArchiveRevision == 0 {
			return nil, ErrLaneIdempotencyConflict
		}
		lane, err = engine.ArchiveRemoteSource(ctx, lane.LaneSessionID, request.Host, archived.ArchiveRevision)
		if err != nil {
			return nil, err
		}
		base["lane"] = publicLaneRecord(lane)
		return base, nil
	case "run", "start", "resume":
		return component.startRemoteLaneCommand(ctx, engine, parent, sourceAttachmentID, request, options, positionals, base)
	default:
		return nil, fmt.Errorf("remote lane command %q is not supported", command)
	}
}

func remoteLaneDoctorReady(roster sharedfederation.Roster, hostID, productID string) (bool, error) {
	product, ok := productcatalog.ProductByID(productID)
	if !ok {
		return false, fmt.Errorf("unsupported lane product %q", productID)
	}
	for _, host := range roster.Hosts {
		if host.ID != hostID {
			continue
		}
		if !sharedfederation.HostSupportsCapability(host, product.LaneCapability) {
			return false, fmt.Errorf("target host %s does not advertise %s", hostID, product.LaneCapability)
		}
		return true, nil
	}
	return false, fmt.Errorf("target host %s is absent from the authoritative federation roster", hostID)
}

func resolveRemoteLaneCommandSelector(
	ctx context.Context,
	engine *LaneEngine,
	sourceAttachmentID string,
	request LaneCommandRequest,
	target string,
	includeArchived, mine bool,
) (LaneRecord, error) {
	target = strings.TrimSpace(target)
	lanes, err := engine.List(ctx, LaneListRequest{
		SourceAttachmentID: sourceAttachmentID, All: includeArchived, Mine: mine,
	})
	if err != nil {
		return LaneRecord{}, err
	}
	matches := make([]LaneRecord, 0, 1)
	for _, lane := range lanes {
		if lane.Product == request.Product && lane.RemoteHostID == request.Host &&
			(lane.LaneSessionID == target || lane.Name == target) {
			matches = append(matches, lane)
		}
	}
	if len(matches) == 0 {
		return LaneRecord{}, ErrLaneNotFound
	}
	if len(matches) > 1 {
		return LaneRecord{}, ErrLaneIdempotencyConflict
	}
	return matches[0], nil
}

//nolint:gocyclo // Canonical lane option normalization and durable remote admission are one boundary.
func (component *federationComponent) startRemoteLaneCommand(
	ctx context.Context,
	engine *LaneEngine,
	parent AttachmentRecord,
	sourceAttachmentID string,
	request LaneCommandRequest,
	options laneCommandOptions,
	positionals []string,
	base map[string]any,
) (map[string]any, error) {
	command := strings.TrimSpace(request.Command)
	if strings.TrimSpace(request.Input) == "" {
		return nil, errors.New("lane prompt is empty")
	}
	var laneID string
	name := strings.TrimSpace(options.name)
	cwd := strings.TrimSpace(options.cwd)
	if cwd == "" {
		cwd = "."
	}
	if command == "resume" {
		if len(positionals) != 1 {
			return nil, errors.New("lane resume requires one session id or unique visible name")
		}
		lane, err := resolveRemoteLaneCommandSelector(ctx, engine, sourceAttachmentID, request, positionals[0], true, false)
		if err != nil {
			return nil, err
		}
		laneID, name, cwd = lane.LaneSessionID, lane.Name, lane.Cwd
	} else {
		if len(positionals) != 0 || name == "" {
			return nil, fmt.Errorf("lane %s requires --name and no target", command)
		}
		var err error
		laneID, err = randomControlRequestID()
		if err != nil {
			return nil, err
		}
	}
	turnID, err := randomControlRequestID()
	if err != nil {
		return nil, err
	}
	requestID, err := randomControlRequestID()
	if err != nil {
		return nil, err
	}
	component.mu.RLock()
	client := component.client
	component.mu.RUnlock()
	permission := laneCommandPermission(request.Product, parent.PermissionMode, options.permissionMode)
	input := map[string]any{"prompt": request.Input, "options": map[string]any{
		"command": command, "arguments": append([]string(nil), options.productArguments...),
		"timeout": options.timeout, "persistent": options.persistent,
	}}
	if err := sharedfederation.ValidateRemoteLaneInput(input); err != nil {
		return nil, err
	}
	state := component.snapshot()
	peerName := parent.Name
	if peerName == "" {
		peerName = parent.SessionID
	}
	parentPeer := sharedfederation.Peer{
		ID: state.HostID + "/" + parent.SessionID, HostID: state.HostID, HostName: state.HostName,
		SessionID: parent.SessionID, GlobalID: sharedfederation.GlobalSessionID(state.HostID, parent.SessionID),
		Name: peerName, DisplayName: sharedfederation.QualifiedPeerName(peerName, state.HostName),
		Entrypoint: parent.Product, InstanceID: parent.AttachmentID, Groups: append([]string(nil), parent.Groups...),
		PermissionMode: parent.PermissionMode, PeerProtocol: sharedfederation.GroupProtocolVersion,
	}
	envelope := sharedfederation.RemoteLaneEnvelope{
		RequestID: requestID, SourceID: parentPeer.ID, TargetHostID: request.Host, Parent: parentPeer,
		Product: request.Product, LaneSessionID: laneID, TurnID: turnID, Name: name, Cwd: cwd,
		Groups: append([]string(nil), options.groups...), InheritParentGroups: options.inheritGroups,
		AllowDuplicateName: options.allowDuplicateName,
		PermissionMode:     permission, InputReference: cloneAttachmentEvidence(input),
	}
	envelopeEvidence, err := remoteLaneEnvelopeEvidence(envelope)
	if err != nil {
		return nil, err
	}
	if client == nil || client.Status().State != sharedfederation.AgentConnected {
		return nil, errors.New("federation hub is disconnected before remote lane acceptance")
	}
	_, _, err = engine.AcceptRemoteSource(ctx, RemoteLaneSourceRequest{
		TargetHostID: request.Host,
		Request: LaneStartRequest{
			LaneSessionID: laneID, TurnID: turnID, SourceAttachmentID: sourceAttachmentID,
			Product: request.Product, Name: name, Cwd: cwd, Groups: append([]string(nil), options.groups...),
			InheritParentGroups: options.inheritGroups, PermissionMode: permission, InputReference: input,
			AllowDuplicateName: options.allowDuplicateName, RemoteRequestID: requestID,
			RemoteEnvelope: envelopeEvidence,
		},
	})
	if err != nil {
		return nil, err
	}
	accepted, err := client.SendRemoteLane(ctx, envelope)
	if err != nil {
		return nil, fmt.Errorf("remote lane acceptance is unconfirmed; durable request %s will not be redispatched automatically: %w", requestID, err)
	}
	if accepted.RequestID != requestID || accepted.LaneSessionID != laneID || accepted.TurnID != turnID {
		return nil, ErrLaneIdempotencyConflict
	}
	lane, turn, err := engine.MarkRemoteSourceRunning(ctx, requestID, accepted.AcceptedRevision)
	if err != nil {
		return nil, err
	}
	base["lane"], base["turn"] = publicLaneRecord(lane), publicLaneTurn(turn)
	if command == "start" {
		return base, nil
	}
	waitCtx, cancel, err := laneCommandWaitContext(ctx, options.timeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	collection, err := engine.Wait(waitCtx, LaneCollectRequest{LaneSessionID: laneID, TurnID: turnID, SourceAttachmentID: sourceAttachmentID})
	if err != nil {
		return nil, err
	}
	base["lane"], base["turn"], base["collection"] = publicLaneRecord(collection.Lane), publicLaneTurn(collection.Turn), publicLaneCollection(collection)
	return base, nil
}

func remoteLaneEnvelopeEvidence(envelope sharedfederation.RemoteLaneEnvelope) (map[string]any, error) {
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode remote lane replay evidence: %w", err)
	}
	var evidence map[string]any
	if err := json.Unmarshal(body, &evidence); err != nil {
		return nil, fmt.Errorf("decode remote lane replay evidence: %w", err)
	}
	return evidence, nil
}

func remoteLaneEnvelopeFromEvidence(evidence map[string]any) (sharedfederation.RemoteLaneEnvelope, error) {
	body, err := json.Marshal(evidence)
	if err != nil {
		return sharedfederation.RemoteLaneEnvelope{}, fmt.Errorf("encode recovered remote lane evidence: %w", err)
	}
	var envelope sharedfederation.RemoteLaneEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return sharedfederation.RemoteLaneEnvelope{}, fmt.Errorf("decode recovered remote lane evidence: %w", err)
	}
	return envelope, nil
}

func (component *federationComponent) publishRemoteLaneNotice(
	ctx context.Context,
	notice sharedfederation.RemoteLaneNotice,
) error {
	component.mu.RLock()
	client := component.client
	component.mu.RUnlock()
	if client == nil {
		return errors.New("federation hub is disconnected")
	}
	lane, err := component.lanes.laneSnapshot(notice.LaneSessionID)
	if err != nil {
		return err
	}
	state := component.snapshot()
	name := lane.Name
	if name == "" {
		name = lane.LaneSessionID
	}
	source := sharedfederation.Peer{
		ID:     state.HostID + "/" + lane.LaneSessionID,
		HostID: state.HostID, HostName: state.HostName, SessionID: lane.LaneSessionID,
		GlobalID: sharedfederation.GlobalSessionID(state.HostID, lane.LaneSessionID),
		Name:     name, DisplayName: sharedfederation.QualifiedPeerName(name, state.HostName),
		Entrypoint: lane.Product, PermissionMode: lane.PermissionMode,
		PeerProtocol: sharedfederation.GroupProtocolVersion, InstanceID: lane.LaneSessionID,
		Groups: append([]string(nil), lane.Groups...), ParentSessionID: lane.ParentSessionID,
	}
	return client.PublishRemoteLaneNotice(ctx, notice, source)
}

func (component *federationComponent) observeLaneTerminal(observation LaneObservation) {
	if observation.Outcome == "" || component.remoteLanes == nil || component.lanes == nil {
		return
	}
	key := laneTurnKey(observation.LaneSessionID, observation.TurnID)
	component.mu.RLock()
	requestID := component.remoteRequests[key]
	component.mu.RUnlock()
	if requestID == "" {
		return
	}
	turn, err := component.lanes.terminalTurn(observation.LaneSessionID, observation.TurnID)
	if err != nil {
		return
	}
	_, err = component.PublishRemoteLaneResult(context.Background(), sharedfederation.RemoteLaneResult{
		RequestID: requestID, LaneSessionID: observation.LaneSessionID, TurnID: observation.TurnID,
		Outcome: observation.Outcome, ResultReference: cloneAttachmentEvidence(turn.ResultReference),
	})
	if err != nil {
		component.mu.Lock()
		component.recoveredTerminalOutbox[requestID] = struct{}{}
		component.mu.Unlock()
		component.scheduleRemoteReconciliation()
	}
}

func (component *federationComponent) start() error {
	component.mu.Lock()
	if component.started {
		component.mu.Unlock()
		return errors.New("embedded federation component is already started")
	}
	component.started = true
	idle := component.client == nil && component.runSession == nil
	var ctx context.Context
	var cancel context.CancelFunc
	var done chan error
	if !idle {
		ctx, cancel = context.WithCancel(context.Background())
		component.cancel = cancel
		component.done = make(chan error, 1)
		done = component.done
		component.lifetimeContext = ctx
	}
	component.mu.Unlock()
	if err := component.publishStatus(); err != nil {
		if cancel != nil {
			cancel()
		}
		component.mu.Lock()
		component.cancel, component.done = nil, nil
		component.lifetimeContext = nil
		component.started = false
		component.mu.Unlock()
		return err
	}
	if idle {
		return nil
	}
	go func() { done <- component.Run(ctx) }()
	return nil
}

// Run maintains the injectable component session until cancellation. The
// production path delegates transport and protocol framing to HostAgent.
func (component *federationComponent) Run(ctx context.Context) error {
	component.mu.Lock()
	component.lifetimeContext = ctx
	component.mu.Unlock()
	defer func() {
		component.mu.Lock()
		component.lifetimeContext = nil
		component.mu.Unlock()
	}()
	if component.client != nil {
		return component.client.Run(ctx)
	}
	if component.runSession == nil {
		<-ctx.Done()
		return nil
	}
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		advertisement := component.currentAdvertisement(ctx)
		if err := component.setHighLevelState(ctx, "connecting", "", "", false); err != nil {
			return err
		}
		sessionErr := component.runSession(ctx, advertisement, func(rosterRevision string) error {
			return component.setHighLevelState(ctx, "connected", "", rosterRevision, true)
		})
		if federationContextEnded(ctx, sessionErr) {
			return nil
		}
		if publishErr := component.setHighLevelState(ctx, "reconnecting", "hub_connection_lost", "", false); publishErr != nil {
			return publishErr
		}
		wait := component.waitRetry
		if wait == nil {
			wait = waitFederationRetry
		}
		if waitErr := wait(ctx, backoff); waitErr != nil {
			if federationContextEnded(ctx, waitErr) {
				return nil
			}
			return waitErr
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
	return nil
}

func federationContextEnded(ctx context.Context, _ error) bool { return ctx.Err() != nil }

// DispatchRemote rejects transport failure before acceptance and never invokes
// the legacy local fallback supplied to the test seam.
func (component *federationComponent) DispatchRemote(
	ctx context.Context,
	request federationDispatchRequest,
) (federationDispatchResult, error) {
	if component.sendRemote == nil {
		return federationDispatchResult{}, &federationDispatchError{
			hubAddress: component.snapshot().HubAddress, cause: errors.New("hub is disconnected"),
		}
	}
	if err := component.sendRemote(ctx, request); err != nil {
		return federationDispatchResult{}, &federationDispatchError{
			hubAddress: component.snapshot().HubAddress, cause: err,
		}
	}
	return federationDispatchResult{Accepted: true}, nil
}

func (component *federationComponent) stop(ctx context.Context) error {
	component.mu.Lock()
	cancel, done := component.cancel, component.done
	component.cancel, component.done = nil, nil
	component.started = false
	component.mu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case err := <-done:
		persistErr := component.persistStatus(ctx, component.snapshot())
		return errors.Join(err, persistErr)
	case <-ctx.Done():
		return fmt.Errorf("stop embedded federation: %w", ctx.Err())
	}
}

func (component *federationComponent) snapshot() FederationStateRecord {
	component.mu.RLock()
	defer component.mu.RUnlock()
	return cloneFederationState(component.status)
}

func (component *federationComponent) currentAdvertisement(ctx context.Context) sharedfederation.HostAdvertisement {
	component.mu.RLock()
	advertisement := component.advertisement
	ready := component.readyProducts
	component.mu.RUnlock()
	if ready != nil {
		advertisement.Products, advertisement.Capabilities = federationProductsAndCapabilities(ready(ctx))
	}
	advertisement.Products = append([]string(nil), advertisement.Products...)
	advertisement.Capabilities = append([]string(nil), advertisement.Capabilities...)
	return advertisement
}

func (component *federationComponent) setHighLevelState(
	ctx context.Context,
	state string,
	errorCode string,
	rosterRevision string,
	connected bool,
) error {
	advertisement := component.currentAdvertisement(ctx)
	component.mu.Lock()
	component.status.Revision++
	component.status.UpdatedAt = component.now().UnixMilli()
	component.status.State = state
	component.status.LastErrorCode = errorCode
	component.status.AdvertisedProducts = append([]string(nil), advertisement.Products...)
	component.status.AdvertisedCapabilities = append([]string(nil), advertisement.Capabilities...)
	if connected {
		component.status.ConnectionGeneration++
		component.status.LastConnectedAt = component.now().UnixMilli()
		component.status.RemoteRosterRevision = rosterRevision
	}
	current := cloneFederationState(component.status)
	publish, observe := component.publishState, component.observe
	component.mu.Unlock()
	if err := component.persistStatus(ctx, current); err != nil {
		return err
	}
	if publish != nil {
		if err := publish(ctx, current); err != nil {
			return err
		}
	}
	if observe != nil {
		observe(current)
	}
	return nil
}

func (component *federationComponent) updateStatus(status sharedfederation.AgentStatus) {
	component.mu.Lock()
	component.status.Revision++
	component.status.UpdatedAt = component.now().UnixMilli()
	component.status.ConnectionGeneration = component.connectionGenerationBase + status.ConnectionGeneration
	component.status.ProtocolVersion = status.ProtocolVersion
	component.status.AdvertisedCapabilities = append([]string(nil), status.Capabilities...)
	component.status.State = string(status.State)
	if status.RemoteRosterGeneration != 0 {
		component.status.RemoteRosterRevision = strconv.FormatUint(status.RemoteRosterGeneration, 10)
	}
	if status.LastConnectedAt != 0 {
		component.status.LastConnectedAt = status.LastConnectedAt
	}
	component.status.LastErrorCode = status.LastErrorCode
	current := cloneFederationState(component.status)
	observe := component.observe
	started := component.started
	component.mu.Unlock()
	if started {
		go component.persistObservedStatus(current)
	}
	if observe != nil {
		observe(current)
	}
}

func (component *federationComponent) publishStatus() error {
	component.mu.RLock()
	current := cloneFederationState(component.status)
	observe := component.observe
	component.mu.RUnlock()
	if err := component.persistStatus(context.Background(), current); err != nil {
		return err
	}
	if observe != nil {
		observe(current)
	}
	return nil
}

func (component *federationComponent) persistObservedStatus(state FederationStateRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = component.persistStatus(ctx, state)
}

func (component *federationComponent) persistStatus(ctx context.Context, state FederationStateRecord) error {
	if component.state == nil {
		return nil
	}
	component.persistMu.Lock()
	defer component.persistMu.Unlock()
	if state.Revision <= component.persistedRecordRevision {
		return nil
	}
	revision, err := component.state.compareAndSwapFederation(ctx, component.stateRevision, state)
	if err != nil {
		return err
	}
	component.stateRevision = revision
	component.persistedRecordRevision = state.Revision
	return nil
}

func (store *StateStore) compareAndSwapFederation(
	ctx context.Context,
	expected statestore.Revision,
	state FederationStateRecord,
) (statestore.Revision, error) {
	if store == nil || store.records == nil {
		return 0, errors.New("federation state requires a state store")
	}
	if err := validateFederationStateRecord(state); err != nil {
		return 0, err
	}
	return store.records.CompareAndSwap(ctx, "federation/state", expected, state)
}

//nolint:gocyclo // Durable federation metadata has one closed validation boundary.
func validateFederationStateRecord(state FederationStateRecord) error {
	if state.SchemaVersion != HostRuntimeSchemaVersion || state.Revision == 0 || state.Generation == 0 ||
		state.CreatedAt <= 0 || state.UpdatedAt < state.CreatedAt {
		return errors.New("federation state has an unsupported schema, revision, generation, or timestamp")
	}
	if strings.TrimSpace(state.HostID) == "" || state.HostID != strings.TrimSpace(state.HostID) ||
		strings.TrimSpace(state.HostName) == "" || state.HostName != strings.TrimSpace(state.HostName) {
		return errors.New("federation state has incomplete host identity")
	}
	if err := validateDaemonHubAddress(state.HubAddress); err != nil {
		return err
	}
	if state.ProtocolVersion <= 0 || state.ConnectionGeneration > 0 && state.HubAddress == "" ||
		state.LastConnectedAt < 0 {
		return errors.New("federation state has invalid connection identity")
	}
	switch state.State {
	case string(sharedfederation.AgentDisabled), string(sharedfederation.AgentConnecting),
		string(sharedfederation.AgentConnected), string(sharedfederation.AgentBackoff),
		string(sharedfederation.AgentIncompatible), "reconnecting":
	default:
		return fmt.Errorf("federation state has unsupported connection state %q", state.State)
	}
	products, capabilities := federationProductsAndCapabilities(state.AdvertisedProducts)
	if !equalFederationStrings(products, state.AdvertisedProducts) ||
		!equalFederationStrings(capabilities, state.AdvertisedCapabilities) {
		return errors.New("federation state has an invalid ready-product advertisement")
	}
	if state.HubAddress != "" && (strings.TrimSpace(state.AdvertisedRuntimeVersion) == "" ||
		strings.TrimSpace(state.AdvertisedRuntimeIdentity) == "") {
		return errors.New("configured federation state lacks advertised runtime identity")
	}
	return nil
}

func equalFederationStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func readyFederationProducts(enabled bool, adapters map[string]LaneAdapter) ([]string, []string) {
	if !enabled {
		return nil, nil
	}
	products := make([]string, 0, len(adapters))
	capabilities := make([]string, 0, len(adapters))
	for product, adapter := range adapters {
		if adapter == nil {
			continue
		}
		descriptor, ok := productcatalog.ProductByID(product)
		if !ok || descriptor.LaneCapability == "" {
			continue
		}
		products = append(products, product)
		capabilities = append(capabilities, descriptor.LaneCapability)
	}
	sort.Strings(products)
	sort.Strings(capabilities)
	return products, capabilities
}

func federationProductsAndCapabilities(values []string) ([]string, []string) {
	seen := make(map[string]struct{}, len(values))
	products := make([]string, 0, len(values))
	capabilities := make([]string, 0, len(values))
	for _, product := range values {
		if _, duplicate := seen[product]; duplicate {
			continue
		}
		descriptor, ok := productcatalog.ProductByID(product)
		if !ok || descriptor.LaneCapability == "" {
			continue
		}
		seen[product] = struct{}{}
		products = append(products, product)
		capabilities = append(capabilities, descriptor.LaneCapability)
	}
	sort.Strings(products)
	sort.Strings(capabilities)
	return products, capabilities
}

func waitFederationRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func federationAttachmentSnapshot(registry *AttachmentRegistry, hostName string) []sharedfederation.Peer {
	records := registry.attachedRecords()
	peers := make([]sharedfederation.Peer, 0, len(records))
	for _, record := range records {
		name := record.Name
		if name == "" {
			name = record.SessionID
		}
		peers = append(peers, sharedfederation.Peer{
			ID:     record.HostID + "/" + record.SessionID,
			HostID: record.HostID, HostName: hostName, SessionID: record.SessionID,
			GlobalID: sharedfederation.GlobalSessionID(record.HostID, record.SessionID),
			Name:     name, DisplayName: sharedfederation.QualifiedPeerName(name, hostName),
			Status: "idle", Cwd: record.Cwd, Entrypoint: record.Product,
			PermissionMode: record.PermissionMode, StartedAt: record.CreatedAt,
			PeerProtocol: sharedfederation.GroupProtocolVersion, InstanceID: record.AttachmentID,
			Groups: append([]string(nil), record.Groups...),
		})
	}
	return peers
}

func cloneFederationState(state FederationStateRecord) FederationStateRecord {
	state.AdvertisedCapabilities = append([]string(nil), state.AdvertisedCapabilities...)
	state.AdvertisedProducts = append([]string(nil), state.AdvertisedProducts...)
	return state
}
