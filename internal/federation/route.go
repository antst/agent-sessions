package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrMessageIdempotencyConflict rejects reuse of a global message ID for
	// different source or frame content.
	ErrMessageIdempotencyConflict = errors.New("federation message id conflicts with admitted delivery")
	// ErrRouteUnauthorized rejects the complete operation before any target is
	// called.
	ErrRouteUnauthorized = errors.New("federation route is not authorized by current global groups")
)

type admittedFrame struct {
	digest string
	ready  chan struct{}
	result AgentFrameResult
	err    error
}

// FrameAdmission provides generation-local idempotency for protocol fixtures
// and hub routing. The host daemon composes its durable delivery ledger around
// the same message identity; this type never writes another durable store.
type FrameAdmission struct {
	mu      sync.Mutex
	records map[string]*admittedFrame
}

// Execute admits one send/broadcast identity exactly once. Exact concurrent or
// later replays receive the first result without invoking dispatch again;
// conflicting content is rejected. Pre-acceptance failures remain retryable.
func (admission *FrameAdmission) Execute(
	ctx context.Context,
	sourceID string,
	frame AgentFrame,
	dispatch func() (AgentFrameResult, error),
) (AgentFrameResult, error) {
	if frame.MessageID == "" {
		return AgentFrameResult{}, errors.New("agent frame requires message_id")
	}
	digest, err := frameAdmissionDigest(sourceID, frame)
	if err != nil {
		return AgentFrameResult{}, err
	}
	admission.mu.Lock()
	if admission.records == nil {
		admission.records = make(map[string]*admittedFrame)
	}
	if prior := admission.records[frame.MessageID]; prior != nil {
		if prior.digest != digest {
			admission.mu.Unlock()
			return AgentFrameResult{}, ErrMessageIdempotencyConflict
		}
		ready := prior.ready
		admission.mu.Unlock()
		select {
		case <-ctx.Done():
			return AgentFrameResult{}, ctx.Err()
		case <-ready:
			return cloneAgentFrameResult(prior.result), prior.err
		}
	}
	record := &admittedFrame{digest: digest, ready: make(chan struct{})}
	admission.records[frame.MessageID] = record
	admission.mu.Unlock()

	result, dispatchErr := dispatch()
	admission.mu.Lock()
	if dispatchErr != nil {
		delete(admission.records, frame.MessageID)
	} else {
		record.result = cloneAgentFrameResult(result)
	}
	record.err = dispatchErr
	close(record.ready)
	admission.mu.Unlock()
	return result, dispatchErr
}

// RouteSnapshot supplies a current global projection at one atomic admission
// boundary.
type RouteSnapshot func(context.Context) ([]Peer, error)

// DeliverPeer calls exactly one already-authorized local or remote adapter.
type DeliverPeer func(context.Context, Peer, Peer, AgentFrame) error

// RouterOptions composes product-neutral routing with caller-owned catalogs and
// delivery authorities.
type RouterOptions struct {
	Snapshot  RouteSnapshot
	Deliver   DeliverPeer
	Admission *FrameAdmission
	Now       func() time.Time
}

// Router resolves the existing single global peer/group space.
type Router struct {
	snapshot  RouteSnapshot
	deliver   DeliverPeer
	admission *FrameAdmission
	now       func() time.Time
}

// HubDeliveryRequest is the transient grouped or terminal-notice admission
// input supplied by the hub transport after a protocol-valid handshake.
type HubDeliveryRequest struct {
	Type         string
	RequestID    string
	SourceHostID string
	SourceID     string
	TargetID     string
	Frame        json.RawMessage
}

// HubDeliveryDecision is an exact current source/target route. The transport
// owns wire clients; this logical decision owns no socket or process.
type HubDeliveryDecision struct {
	RequestID    string
	Type         string
	SourceHostID string
	TargetHostID string
	Source       Peer
	Target       Peer
	Frame        json.RawMessage
}

// ResolveHubDelivery performs destination-local grouped admission against one
// registry snapshot before the transport records or forwards the request.
func ResolveHubDelivery(snapshot RegistrySnapshot, request HubDeliveryRequest) (HubDeliveryDecision, error) {
	frame, err := parseHubDeliveryRequest(request)
	if err != nil {
		return HubDeliveryDecision{}, err
	}
	peers := indexHubDeliveryPeers(snapshot.Peers)
	target, targetExists := peers[request.TargetID]
	if !targetExists {
		return HubDeliveryDecision{}, fmt.Errorf("target peer %s is not live", request.TargetID)
	}
	source, err := resolveHubDeliverySource(peers, target, request, frame)
	if err != nil {
		return HubDeliveryDecision{}, err
	}
	if err := ValidateCurrentDeliveryGroups(source, target, frame); err != nil {
		return HubDeliveryDecision{}, err
	}
	return HubDeliveryDecision{
		RequestID: request.RequestID, Type: request.Type,
		SourceHostID: request.SourceHostID, TargetHostID: target.HostID,
		Source: clonePeer(source), Target: clonePeer(target), Frame: append(json.RawMessage(nil), request.Frame...),
	}, nil
}

func parseHubDeliveryRequest(request HubDeliveryRequest) (AgentFrame, error) {
	if request.RequestID == "" || request.SourceID == "" || request.TargetID == "" || len(request.Frame) == 0 {
		return AgentFrame{}, errors.New("federated delivery requires request, source, target, and frame")
	}
	if request.Type != "group_deliver" && request.Type != "terminal_notice_deliver" {
		return AgentFrame{}, fmt.Errorf("unsupported hub delivery type %q", request.Type)
	}
	var frame AgentFrame
	if json.Unmarshal(request.Frame, &frame) != nil || frame.Version != AgentFrameVersion ||
		frame.Type != "delivery" || frame.Source == nil || frame.Source.ID != request.SourceID ||
		frame.SourceSessionID != frame.Source.SessionID {
		return AgentFrame{}, errors.New("federated delivery contains an invalid agent frame")
	}
	return frame, nil
}

func indexHubDeliveryPeers(snapshot []Peer) map[string]Peer {
	peers := make(map[string]Peer, len(snapshot))
	for _, peer := range snapshot {
		peers[peer.ID] = peer
	}
	return peers
}

func resolveHubDeliverySource(
	peers map[string]Peer,
	target Peer,
	request HubDeliveryRequest,
	frame AgentFrame,
) (Peer, error) {
	if request.Type == "group_deliver" {
		source, sourceExists := peers[request.SourceID]
		if !sourceExists || source.HostID != request.SourceHostID {
			return Peer{}, errors.New("grouped source is not advertised by its sending host")
		}
		if frame.Source.ID != source.ID || frame.SourceSessionID != source.SessionID {
			return Peer{}, errors.New("grouped delivery frame source does not match the registry")
		}
		return source, nil
	}
	source := clonePeer(*frame.Source)
	if source.HostID != request.SourceHostID {
		return Peer{}, errors.New("terminal notice source is not owned by the sending host")
	}
	if err := ValidateSnapshotPeer(source, request.SourceHostID); err != nil {
		return Peer{}, err
	}
	if target.HostID == request.SourceHostID {
		return Peer{}, errors.New("terminal notice target must be on another host")
	}
	return source, nil
}

// DeliveryRoute contains only the correlated metadata needed to authenticate
// a delivery ACK/error. It never retains frame content.
type DeliveryRoute struct {
	RequestID           string `json:"request_id"`
	SourceHostID        string `json:"source_host_id"`
	SourceID            string `json:"source_id"`
	TargetHostID        string `json:"target_host_id"`
	TargetID            string `json:"target_id"`
	RemoteLaneRequestID string `json:"remote_lane_request_id,omitempty"`
}

// DeliveryRouteCounts is the bounded metadata-only hub routing projection.
type DeliveryRouteCounts struct {
	Pending int `json:"pending"`
}

// DeliveryRouteTable authenticates transient delivery outcomes and disconnect
// cleanup. Host lifecycle remains outside this table.
type DeliveryRouteTable struct {
	mu     sync.Mutex
	routes map[string]DeliveryRoute
}

// Begin records one accepted exact route before its frame is forwarded.
func (table *DeliveryRouteTable) Begin(route DeliveryRoute) error {
	if route.RequestID == "" || route.SourceHostID == "" || route.SourceID == "" ||
		route.TargetHostID == "" || route.TargetID == "" {
		return errors.New("delivery route identity is incomplete")
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.routes == nil {
		table.routes = make(map[string]DeliveryRoute)
	}
	if _, duplicate := table.routes[route.RequestID]; duplicate {
		return errors.New("duplicate federated delivery request")
	}
	table.routes[route.RequestID] = route
	return nil
}

// Resolve authenticates and consumes an ACK/error only from the exact target
// host and peer. A forged or late outcome leaves a current route intact.
func (table *DeliveryRouteTable) Resolve(
	requestID, outcomeHostID, sourceID, targetID string,
) (DeliveryRoute, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, exists := table.routes[requestID]
	if !exists || route.TargetHostID != outcomeHostID || route.SourceID != sourceID || route.TargetID != targetID {
		return DeliveryRoute{}, false
	}
	delete(table.routes, requestID)
	return route, true
}

// DropHost removes every route involving a disconnected host and returns only
// routes whose still-connected source needs a terminal destination failure.
func (table *DeliveryRouteTable) DropHost(hostID string) []DeliveryRoute {
	table.mu.Lock()
	defer table.mu.Unlock()
	failed := make([]DeliveryRoute, 0)
	for requestID, route := range table.routes {
		if route.SourceHostID != hostID && route.TargetHostID != hostID {
			continue
		}
		delete(table.routes, requestID)
		if route.TargetHostID == hostID && route.SourceHostID != hostID {
			failed = append(failed, route)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].RequestID < failed[j].RequestID })
	return failed
}

// Counts returns content-free bounded hub status.
func (table *DeliveryRouteTable) Counts() DeliveryRouteCounts {
	table.mu.Lock()
	defer table.mu.Unlock()
	return DeliveryRouteCounts{Pending: len(table.routes)}
}

// NewRouter creates a routing authority without owning durable host state.
func NewRouter(options RouterOptions) (*Router, error) {
	if options.Snapshot == nil || options.Deliver == nil {
		return nil, errors.New("federation router requires snapshot and delivery authorities")
	}
	if options.Admission == nil {
		options.Admission = &FrameAdmission{}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Router{
		snapshot: options.Snapshot, deliver: options.Deliver,
		admission: options.Admission, now: options.Now,
	}, nil
}

// Route validates and resolves discover, send, and global-group broadcast.
func (router *Router) Route(ctx context.Context, sourceID string, frame AgentFrame) (AgentFrameResult, error) {
	if frame.Version != AgentFrameVersion {
		return AgentFrameResult{}, errors.New("agent frame protocol is incompatible")
	}
	if strings.TrimSpace(frame.MessageID) == "" {
		return AgentFrameResult{}, errors.New("agent frame requires message_id")
	}
	if len(frame.Content) > MaxAgentFrameBytes {
		return AgentFrameResult{}, errors.New("agent frame content exceeds 1 MiB")
	}
	if frame.Type == "discover" {
		return router.routeOnce(ctx, sourceID, frame)
	}
	return router.admission.Execute(ctx, sourceID, frame, func() (AgentFrameResult, error) {
		return router.routeOnce(ctx, sourceID, frame)
	})
}

//nolint:gocyclo // The three closed AgentFrame operations share one atomic snapshot.
func (router *Router) routeOnce(ctx context.Context, sourceID string, frame AgentFrame) (AgentFrameResult, error) {
	peers, err := router.snapshot(ctx)
	if err != nil {
		return AgentFrameResult{}, err
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	var source Peer
	foundSource := false
	for _, peer := range peers {
		if peer.ID == sourceID || peer.SessionID == sourceID {
			source, foundSource = clonePeer(peer), true
			break
		}
	}
	if !foundSource {
		return AgentFrameResult{}, errors.New("source session is not a live registered peer")
	}
	result := AgentFrameResult{Version: AgentFrameVersion, MessageID: frame.MessageID}
	switch frame.Type {
	case "discover":
		if len(frame.Targets) != 0 || frame.Group != "" || frame.Content != "" {
			return AgentFrameResult{}, errors.New("discover frame contains routing fields")
		}
		result.Type = "discover.result"
		for _, peer := range peers {
			if peer.ID != source.ID && peerGroupsIntersect(source, peer) {
				result.Peers = append(result.Peers, clonePeer(peer))
			}
		}
		return result, nil
	case "send":
		if len(frame.Targets) == 0 || frame.Group != "" || frame.Content == "" {
			return AgentFrameResult{}, errors.New("send requires targets and content")
		}
		if hasDuplicateStrings(frame.Targets) {
			return AgentFrameResult{}, errors.New("send targets contain duplicates")
		}
		visible := visiblePeers(source, peers)
		targets := make([]Peer, 0, len(frame.Targets))
		seen := make(map[string]struct{}, len(frame.Targets))
		for _, requested := range frame.Targets {
			target, resolveErr := ResolveGroupedTarget(requested, visible)
			if resolveErr != nil {
				return AgentFrameResult{}, resolveErr
			}
			if _, duplicate := seen[target.ID]; duplicate {
				return AgentFrameResult{}, errors.New("send targets resolve to a duplicate peer")
			}
			seen[target.ID] = struct{}{}
			targets = append(targets, target)
		}
		result.Type = "send.result"
		result.Deliveries = router.deliverAll(ctx, source, targets, frame)
		return result, nil
	case "broadcast":
		if frame.Group == "" || len(frame.Targets) != 0 || frame.Content == "" {
			return AgentFrameResult{}, errors.New("broadcast requires group and content")
		}
		if !peerHasGroup(source, frame.Group) {
			return AgentFrameResult{}, errors.New("broadcast sender is not a member of the group")
		}
		targets := make([]Peer, 0, len(peers))
		for _, peer := range peers {
			if peer.ID != source.ID && peerHasGroup(peer, frame.Group) {
				targets = append(targets, peer)
			}
		}
		result.Type = "broadcast.result"
		result.Deliveries = router.deliverAll(ctx, source, targets, frame)
		return result, nil
	default:
		return AgentFrameResult{}, fmt.Errorf("unsupported agent frame type %q", frame.Type)
	}
}

func (router *Router) deliverAll(
	ctx context.Context,
	source Peer,
	targets []Peer,
	request AgentFrame,
) []DeliveryResult {
	delivered := AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: request.MessageID,
		SourceSessionID: source.SessionID, Source: &source, Content: request.Content,
		Group: request.Group, Summary: request.Summary, SentAt: request.SentAt,
	}
	if delivered.SentAt == "" {
		delivered.SentAt = router.now().UTC().Format(time.RFC3339Nano)
	}
	results := make([]DeliveryResult, len(targets))
	var wait sync.WaitGroup
	for index, target := range targets {
		index, target := index, clonePeer(target)
		wait.Add(1)
		go func() {
			defer wait.Done()
			result := DeliveryResult{Target: target.ID, SessionID: target.SessionID, Status: "accepted"}
			if err := router.deliver(ctx, source, target, delivered); err != nil {
				result.Status, result.Error = "failed", err.Error()
			}
			results[index] = result
		}()
	}
	wait.Wait()
	return results
}

// ResolveGroupedTarget resolves exact global/session/name/host-suffixed forms
// and fails rather than guessing when an unqualified name is ambiguous.
func ResolveGroupedTarget(raw string, peers []Peer) (Peer, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "session:")
	if raw == "" {
		return Peer{}, errors.New("empty peer target")
	}
	matches := make([]Peer, 0, 1)
	for _, peer := range peers {
		if raw == peer.ID || raw == peer.GlobalID || raw == peer.SessionID || raw == peer.Name || raw == peer.DisplayName {
			matches = append(matches, peer)
		}
	}
	if len(matches) == 0 {
		return Peer{}, fmt.Errorf("target peer %q is not live in a shared group", raw)
	}
	if len(matches) > 1 {
		return Peer{}, fmt.Errorf("target peer %q is ambiguous; use name--host or session id", raw)
	}
	return clonePeer(matches[0]), nil
}

// ValidateCurrentDeliveryGroups rechecks destination-local group authority at
// remote admission time, after the source host's earlier route decision.
func ValidateCurrentDeliveryGroups(source, target Peer, frame AgentFrame) error {
	if frame.Group != "" {
		if !peerHasGroup(source, frame.Group) || !peerHasGroup(target, frame.Group) {
			return errors.New("federated broadcast peers are not current members of the named group")
		}
		return nil
	}
	if !peerGroupsIntersect(source, target) {
		return errors.New("federated peers do not share a group")
	}
	return nil
}

func visiblePeers(source Peer, peers []Peer) []Peer {
	result := make([]Peer, 0, len(peers))
	for _, peer := range peers {
		if peer.ID != source.ID && peerGroupsIntersect(source, peer) {
			result = append(result, peer)
		}
	}
	return result
}

func peerGroupsIntersect(left, right Peer) bool {
	for _, group := range left.Groups {
		if peerHasGroup(right, group) {
			return true
		}
	}
	return false
}

func peerHasGroup(peer Peer, group string) bool {
	for _, candidate := range peer.Groups {
		if candidate == group {
			return true
		}
	}
	return false
}

func hasDuplicateStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func frameAdmissionDigest(sourceID string, frame AgentFrame) (string, error) {
	body, err := json.Marshal(struct {
		SourceID string     `json:"source_id"`
		Frame    AgentFrame `json:"frame"`
	}{SourceID: sourceID, Frame: frame})
	if err != nil {
		return "", fmt.Errorf("encode federation message identity: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func cloneAgentFrameResult(result AgentFrameResult) AgentFrameResult {
	result.Peers = append([]Peer(nil), result.Peers...)
	for index := range result.Peers {
		result.Peers[index] = clonePeer(result.Peers[index])
	}
	result.Deliveries = append([]DeliveryResult(nil), result.Deliveries...)
	return result
}
