package federation

import (
	"errors"
	"fmt"
	"sync"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// RemoteLaneRoute is the bounded metadata-only ownership retained while one
// accepted remote lane request is in flight through the central hub.
type RemoteLaneRoute struct {
	RequestID            string
	SourceHostID         string
	SourceID             string
	TargetHostID         string
	ParentSessionID      string
	LaneSessionID        string
	TurnID               string
	Fingerprint          string
	TerminalAcknowledged bool
}

// RemoteLaneRouteDecision is the exact destination decision returned before
// the transport forwards lane_exec.
type RemoteLaneRouteDecision struct {
	Route    RemoteLaneRoute
	Envelope RemoteLaneEnvelope
}

// RemoteLaneRouteTable authenticates lane exec/cancel/outcome ownership. It
// retains identities only; native state, prompts and results remain on hosts.
type RemoteLaneRouteTable struct {
	mu     sync.Mutex
	routes map[string]RemoteLaneRoute
}

// RemoteLaneArchiveRoute is bounded ownership for one destination archive
// transaction. It contains identities only and is discarded after one exact
// acknowledgement or refusal.
type RemoteLaneArchiveRoute struct {
	RequestID     string
	SourceHostID  string
	TargetHostID  string
	SourceID      string
	LaneSessionID string
	Product       string
}

// RemoteLaneArchiveRouteTable authenticates remote archive request/result
// ownership independently from the already-finalized turn route.
type RemoteLaneArchiveRouteTable struct {
	mu     sync.Mutex
	routes map[string]RemoteLaneArchiveRoute
}

// Begin validates the current source peer and destination capability before
// forwarding a remote archive request.
//
//nolint:gocyclo // Source, destination, capability, and idempotency are one fail-closed authorization gate.
func (table *RemoteLaneArchiveRouteTable) Begin(
	snapshot RegistrySnapshot,
	sourceHostID string,
	request RemoteLaneArchive,
) (RemoteLaneArchiveRoute, error) {
	if request.RequestID == "" || request.RemoteRequestID == "" || request.SourceID == "" ||
		request.TargetHostID == "" || request.Product == "" || request.LaneSessionID == "" {
		return RemoteLaneArchiveRoute{}, errors.New("remote lane archive identity is incomplete")
	}
	product, ok := productcatalog.ProductByID(request.Product)
	if !ok {
		return RemoteLaneArchiveRoute{}, fmt.Errorf("unsupported remote lane product %q", request.Product)
	}
	foundSource := false
	for _, peer := range snapshot.Peers {
		if peer.ID == request.SourceID && peer.HostID == sourceHostID {
			foundSource = true
			break
		}
	}
	foundTarget := false
	for _, host := range snapshot.Hosts {
		if host.ID == request.TargetHostID && HostSupportsCapability(host, product.LaneCapability) {
			foundTarget = true
			break
		}
	}
	if !foundSource || !foundTarget {
		return RemoteLaneArchiveRoute{}, errors.New("remote lane archive source or target is unavailable")
	}
	route := RemoteLaneArchiveRoute{
		RequestID: request.RequestID, SourceHostID: sourceHostID, TargetHostID: request.TargetHostID,
		SourceID: request.SourceID, LaneSessionID: request.LaneSessionID, Product: request.Product,
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.routes == nil {
		table.routes = make(map[string]RemoteLaneArchiveRoute)
	}
	if prior, exists := table.routes[route.RequestID]; exists {
		if prior != route {
			return RemoteLaneArchiveRoute{}, ErrRemoteLaneIdempotencyConflict
		}
		return prior, nil
	}
	table.routes[route.RequestID] = route
	return route, nil
}

// Complete authenticates one destination result and consumes the archive
// route exactly once.
func (table *RemoteLaneArchiveRouteTable) Complete(requestID, targetHostID string) (RemoteLaneArchiveRoute, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, ok := table.routes[requestID]
	if !ok || route.TargetHostID != targetHostID {
		return RemoteLaneArchiveRoute{}, false
	}
	delete(table.routes, requestID)
	return route, true
}

// DropHost releases only transient archive correlations. Destination native
// archive remains idempotent, so a source may retry with a fresh correlation
// after either connection returns.
func (table *RemoteLaneArchiveRouteTable) DropHost(hostID string) {
	table.mu.Lock()
	defer table.mu.Unlock()
	for requestID, route := range table.routes {
		if route.SourceHostID == hostID || route.TargetHostID == hostID {
			delete(table.routes, requestID)
		}
	}
}

// Begin validates the current global source and target capability, then
// records exact ownership before forwarding.
//
//nolint:gocyclo // Fail-closed source, parent, product, host and duplicate admission is one transaction.
func (table *RemoteLaneRouteTable) Begin(
	snapshot RegistrySnapshot,
	sourceHostID string,
	envelope RemoteLaneEnvelope,
) (RemoteLaneRouteDecision, error) {
	if envelope.RequestID == "" || envelope.SourceID == "" || envelope.TargetHostID == "" ||
		envelope.LaneSessionID == "" || envelope.TurnID == "" || envelope.Parent.ID != envelope.SourceID {
		return RemoteLaneRouteDecision{}, errors.New("remote lane route identity is incomplete")
	}
	var source Peer
	foundSource := false
	for _, peer := range snapshot.Peers {
		if peer.ID == envelope.SourceID {
			source, foundSource = peer, true
			break
		}
	}
	if !foundSource || source.HostID != sourceHostID || envelope.Parent.HostID != sourceHostID ||
		envelope.Parent.SessionID != source.SessionID || envelope.Parent.InstanceID != source.InstanceID {
		return RemoteLaneRouteDecision{}, ErrRemoteLaneParentMismatch
	}
	product, ok := productcatalog.ProductByID(envelope.Product)
	if !ok {
		return RemoteLaneRouteDecision{}, fmt.Errorf("unsupported remote lane product %q", envelope.Product)
	}
	var target Host
	foundTarget := false
	for _, host := range snapshot.Hosts {
		if host.ID == envelope.TargetHostID {
			target, foundTarget = host, true
			break
		}
	}
	if !foundTarget || !HostSupportsCapability(target, product.LaneCapability) {
		return RemoteLaneRouteDecision{}, errors.New("target host does not advertise the requested lane capability")
	}
	forwarded := cloneRemoteLaneEnvelope(envelope)
	forwarded.SourceID = source.ID
	forwarded.Parent = clonePeer(source)
	if err := ValidateRemoteLaneInput(forwarded.InputReference); err != nil {
		return RemoteLaneRouteDecision{}, err
	}
	fingerprint, err := remoteLaneFingerprint(forwarded)
	if err != nil {
		return RemoteLaneRouteDecision{}, err
	}
	route := RemoteLaneRoute{
		RequestID: envelope.RequestID, SourceHostID: sourceHostID, SourceID: source.ID,
		TargetHostID: envelope.TargetHostID, ParentSessionID: source.SessionID,
		LaneSessionID: envelope.LaneSessionID, TurnID: envelope.TurnID, Fingerprint: fingerprint,
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.routes == nil {
		table.routes = make(map[string]RemoteLaneRoute)
	}
	if prior, duplicate := table.routes[route.RequestID]; duplicate {
		priorIdentity := prior
		priorIdentity.TerminalAcknowledged = false
		if priorIdentity != route {
			return RemoteLaneRouteDecision{}, ErrRemoteLaneIdempotencyConflict
		}
		return RemoteLaneRouteDecision{Route: prior, Envelope: forwarded}, nil
	}
	table.routes[route.RequestID] = route
	return RemoteLaneRouteDecision{Route: route, Envelope: forwarded}, nil
}

// Cancel authenticates a cancellation from the exact originating host while
// retaining the route for its eventual terminal outcome.
func (table *RemoteLaneRouteTable) Cancel(requestID, sourceHostID string) (RemoteLaneRoute, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, ok := table.routes[requestID]
	if !ok || route.SourceHostID != sourceHostID {
		return RemoteLaneRoute{}, false
	}
	return route, true
}

// CancellationDecision authenticates a cancellation ACK/refusal from the
// exact destination while retaining the route for its terminal outcome.
func (table *RemoteLaneRouteTable) CancellationDecision(requestID, targetHostID string) (RemoteLaneRoute, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, ok := table.routes[requestID]
	if !ok || route.TargetHostID != targetHostID {
		return RemoteLaneRoute{}, false
	}
	return route, true
}

// ResultDecision authenticates a result ACK/refusal from the exact source
// host while retaining ownership for the following terminal notice.
func (table *RemoteLaneRouteTable) ResultDecision(requestID, sourceHostID string) (RemoteLaneRoute, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, ok := table.routes[requestID]
	if !ok || route.SourceHostID != sourceHostID {
		return RemoteLaneRoute{}, false
	}
	return route, true
}

// Outcome authenticates an exact destination response. Terminal responses
// consume the route; progress fragments do not.
func (table *RemoteLaneRouteTable) Outcome(requestID, targetHostID string, terminal bool) (RemoteLaneRoute, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, ok := table.routes[requestID]
	if !ok || route.TargetHostID != targetHostID {
		return RemoteLaneRoute{}, false
	}
	if terminal {
		delete(table.routes, requestID)
	}
	return route, true
}

// AuthorizeNotice resolves but does not consume the route represented by one
// content-free terminal notice. The route remains owned until the exact
// source parent acknowledges delivery, so a reconnect cannot turn accepted
// work into a false terminal failure.
func (table *RemoteLaneRouteTable) AuthorizeNotice(
	sourceHostID, laneSessionID, targetHostID, targetSessionID string,
) (RemoteLaneRoute, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	for _, route := range table.routes {
		if route.TargetHostID == sourceHostID && route.LaneSessionID == laneSessionID &&
			route.SourceHostID == targetHostID && route.ParentSessionID == targetSessionID {
			return route, true
		}
	}
	return RemoteLaneRoute{}, false
}

// CompleteNotice consumes a terminal route only after the exact parent host
// acknowledged the corresponding content-free notice.
func (table *RemoteLaneRouteTable) CompleteNotice(requestID, sourceHostID, targetHostID string) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, ok := table.routes[requestID]
	if !ok || route.TargetHostID != sourceHostID || route.SourceHostID != targetHostID {
		return false
	}
	route.TerminalAcknowledged = true
	table.routes[requestID] = route
	return true
}

// FinalizeNotice releases an acknowledged route only after the hub has
// delivered that acknowledgement back to the exact destination host. If the
// write is lost, the acknowledged tombstone remains available for retry.
func (table *RemoteLaneRouteTable) FinalizeNotice(requestID, sourceHostID, targetHostID string) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	route, ok := table.routes[requestID]
	if !ok || !route.TerminalAcknowledged || route.TargetHostID != sourceHostID || route.SourceHostID != targetHostID {
		return false
	}
	delete(table.routes, requestID)
	return true
}

// DropHost preserves accepted request ownership across transient source or
// destination daemon reconnects. Terminal evidence or an explicit hub
// restart, not a socket lifetime, ends the route.
func (table *RemoteLaneRouteTable) DropHost(hostID string) []RemoteLaneRoute {
	_ = hostID
	return nil
}

// Counts returns only bounded routing metadata.
func (table *RemoteLaneRouteTable) Counts() int {
	table.mu.Lock()
	defer table.mu.Unlock()
	active := 0
	for _, route := range table.routes {
		if !route.TerminalAcknowledged {
			active++
		}
	}
	return active
}
