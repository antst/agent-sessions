package federator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

// AgentServiceRecord returns the one Claude-native service record published
// by the host agent. Private Claude profiles project this same record; they do
// not create per-peer router identities.
func AgentServiceRecord(runtimeDir string) ([]byte, error) {
	response, err := requestAgentControl(runtimeDir, Message{Type: "service_record", Version: GroupProtocolVersion})
	if err != nil {
		return nil, err
	}
	if len(response.Data) == 0 {
		return nil, errors.New("agent returned no service record")
	}
	return append([]byte(nil), response.Data...), nil
}

// ProcessStart returns the platform process-start identity used by live peer
// registration. It is exposed only for product supershims in this module.
func ProcessStart(pid int) string { return processStart(pid) }

// PeerRegistration binds one live product-owned session to its delivery adapter.
type PeerRegistration struct {
	Version            int    `json:"version"`
	SessionID          string `json:"session_id"`
	Product            string `json:"product"`
	Name               string `json:"name"`
	Status             string `json:"status,omitempty"`
	PermissionMode     string `json:"permission_mode,omitempty"`
	Cwd                string `json:"cwd,omitempty"`
	PID                int    `json:"pid"`
	ProcStart          string `json:"proc_start"`
	Socket             string `json:"socket"`
	LifecyclePID       int    `json:"lifecycle_pid,omitempty"`
	LifecycleProcStart string `json:"lifecycle_proc_start,omitempty"`
	PrivateConfigRoot  string `json:"private_config_root,omitempty"`
	StartedAt          int64  `json:"started_at,omitempty"`
}

// ParentContext is the agent-attested parent layer inherited by a child lane.
// Process identity is lifecycle authority; session identity and groups remain
// durable even when a persistent child outlives that process.
type ParentContext struct {
	HostID           string   `json:"host_id"`
	SessionID        string   `json:"session_id"`
	Product          string   `json:"product"`
	InstanceID       string   `json:"instance_id"`
	Groups           []string `json:"groups"`
	AlwaysApprove    bool     `json:"always_approve"`
	AdapterPID       int      `json:"adapter_pid,omitempty"`
	AdapterProcStart string   `json:"adapter_proc_start,omitempty"`
	AdapterSocket    string   `json:"adapter_socket,omitempty"`
	PID              int      `json:"pid"`
	ProcStart        string   `json:"proc_start"`
	PermissionMode   string   `json:"permission_mode"`
}

// ResolveParentContext returns the exact live local registration and durable
// preferences for a parent session. It never resolves a remote peer.
func ResolveParentContext(runtimeDir, sessionID string) (ParentContext, error) {
	response, err := requestAgentControl(runtimeDir, Message{
		Type: "parent_context", Version: GroupProtocolVersion, SessionID: sessionID,
	})
	if err != nil {
		return ParentContext{}, err
	}
	if response.ParentContext == nil {
		return ParentContext{}, errors.New("agent returned no parent context")
	}
	return *response.ParentContext, nil
}

// RegisterPeer installs or refreshes one live adapter registration.
func RegisterPeer(runtimeDir string, registration PeerRegistration) (Peer, error) {
	response, err := requestAgentControl(runtimeDir, Message{Type: "peer_register", Registration: &registration})
	if err != nil {
		return Peer{}, err
	}
	if len(response.Peers) != 1 {
		return Peer{}, errors.New("agent registration returned no peer")
	}
	return response.Peers[0], nil
}

// UpdatePeerStatus refreshes mutable live presentation without changing preferences.
func UpdatePeerStatus(runtimeDir string, registration PeerRegistration) (Peer, error) {
	response, err := requestAgentControl(runtimeDir, Message{Type: "peer_update", Registration: &registration})
	if err != nil {
		return Peer{}, err
	}
	if len(response.Peers) != 1 {
		return Peer{}, errors.New("agent update returned no peer")
	}
	return response.Peers[0], nil
}

// UnregisterPeer removes only the exact live adapter registration.
func UnregisterPeer(runtimeDir string, registration PeerRegistration) error {
	_, err := requestAgentControl(runtimeDir, Message{Type: "peer_unregister", Registration: &registration})
	return err
}

func requestAgentControl(runtimeDir string, request Message) (Message, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	conn, err := net.DialTimeout("unix", filepath.Join(runtimeDir, "agent.sock"), time.Second)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = conn.Close() }()
	// Routing may wait for one remote delivery acknowledgement. Fan-out is
	// concurrent, so one acknowledgement window plus transport margin bounds
	// every control request regardless of recipient count.
	_ = conn.SetDeadline(time.Now().Add(wireWriteTimeout + 5*time.Second))
	if err := newWireConn(conn).Send(request); err != nil {
		return Message{}, err
	}
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return Message{}, err
	}
	if response.Type == "error" {
		return Message{}, errors.New(defaultString(response.Error, "agent control failed"))
	}
	return response, nil
}

//nolint:gocyclo // Registration validates adapter, lifecycle, catalog, and replacement identity together.
func (a *agent) registerPeer(registration PeerRegistration, updateOnly bool) (Peer, error) {
	if registration.Version != GroupProtocolVersion || !validCatalogSessionID(registration.SessionID) ||
		!validProduct(registration.Product) || registration.Name == "" || registration.PID <= 1 ||
		registration.ProcStart == "" || registration.Socket == "" {
		return Peer{}, errors.New("invalid peer registration")
	}
	adapterInfo := procinfo.Read(registration.PID)
	if adapterInfo.Status != procinfo.Known || adapterInfo.Start != registration.ProcStart || !processLive(registration.PID) {
		return Peer{}, errors.New("peer registration process identity is not live")
	}
	if !probeUnix(registration.Socket, 250*time.Millisecond) {
		return Peer{}, errors.New("peer registration socket is not reachable")
	}
	lifecyclePID, lifecycleProcStart := registration.LifecyclePID, registration.LifecycleProcStart
	if lifecyclePID == 0 && lifecycleProcStart == "" {
		lifecyclePID, lifecycleProcStart = registration.PID, registration.ProcStart
	}
	lifecycleInfo := procinfo.Read(lifecyclePID)
	if lifecyclePID <= 1 || lifecycleProcStart == "" || lifecycleInfo.Status != procinfo.Known ||
		lifecycleInfo.Start != lifecycleProcStart || !processLive(lifecyclePID) {
		return Peer{}, errors.New("peer lifecycle process identity is not live")
	}
	if registration.PrivateConfigRoot != "" {
		expected := ClaudePeerPrivateRoot(a.options.HostID, registration.SessionID)
		if registration.Product != "claude" || filepath.Clean(registration.PrivateConfigRoot) != filepath.Clean(expected) {
			return Peer{}, errors.New("peer private configuration root is not agent-owned")
		}
	}
	preference, groups, ok, err := a.catalog.get(registration.SessionID)
	if err != nil {
		return Peer{}, err
	}
	if !ok || preference.Product != registration.Product {
		return Peer{}, errors.New("peer registration has no matching session preferences")
	}
	if preference.AlwaysApprove != (registration.PermissionMode == "bypassPermissions") {
		return Peer{}, errors.New("peer permission mode does not match durable yolo preference")
	}
	id := a.options.HostID + "/" + registration.SessionID
	a.mu.Lock()
	if _, retiring := a.retirements[id]; retiring {
		a.mu.Unlock()
		return Peer{}, errors.New("session adapter retirement is still pending")
	}
	current, exists := a.local[id]
	if updateOnly && !exists {
		a.mu.Unlock()
		return Peer{}, errors.New("peer is not registered")
	}
	if exists && (current.PID != registration.PID || current.ProcStart != registration.ProcStart) {
		if processStart(current.PID) == current.ProcStart && processLive(current.PID) {
			a.mu.Unlock()
			return Peer{}, errors.New("session already has a different live adapter")
		}
	}
	startedAt := registration.StartedAt
	if startedAt == 0 {
		startedAt = time.Now().UnixMilli()
	}
	peer := Peer{
		ID: id, HostID: a.options.HostID, HostName: a.options.HostName,
		SessionID: registration.SessionID, GlobalID: globalSessionID(a.options.HostID, registration.SessionID),
		Name: cleanPeerName(registration.Name), DisplayName: qualifiedName(registration.Name, a.options.HostName),
		Status: defaultString(registration.Status, "idle"), Cwd: registration.Cwd,
		Entrypoint: registration.Product, PermissionMode: defaultString(registration.PermissionMode, "default"),
		StartedAt: startedAt, PeerProtocol: GroupProtocolVersion,
		InstanceID: sessionKey(fmt.Sprintf("%s\x00%d\x00%s", a.options.HostID, registration.PID, registration.ProcStart)),
		Groups:     groups, ParentSessionID: preference.ParentSession,
	}
	a.local[id] = localPeer{
		Peer: peer, PID: registration.PID, ProcStart: registration.ProcStart,
		Socket: registration.Socket, GroupProtocol: GroupProtocolVersion,
		LifecyclePID: lifecyclePID, LifecycleProcStart: lifecycleProcStart,
		AdapterStrongStart: adapterInfo.StrongStart, LifecycleStrongStart: lifecycleInfo.StrongStart,
		PrivateConfigRoot: registration.PrivateConfigRoot,
	}
	a.mu.Unlock()
	a.signalLocalChanged()
	return peer, nil
}

func (a *agent) unregisterPeer(registration PeerRegistration) error {
	if registration.SessionID == "" || registration.PID <= 1 || registration.ProcStart == "" {
		return errors.New("invalid peer unregister request")
	}
	id := a.options.HostID + "/" + registration.SessionID
	a.mu.Lock()
	current, exists := a.local[id]
	removed := false
	if exists && current.PID == registration.PID && current.ProcStart == registration.ProcStart {
		delete(a.local, id)
		removed = true
	}
	a.mu.Unlock()
	if removed {
		a.signalLocalChanged()
	}
	return nil
}

func (a *agent) parentContext(sessionID string) (ParentContext, error) {
	a.reconcileRegisteredPeers()
	a.mu.RLock()
	peer, ok := localPeerBySession(a.local, sessionID)
	a.mu.RUnlock()
	if !ok {
		return ParentContext{}, errors.New("parent session is not a live local peer")
	}
	preference, groups, exists, err := a.catalog.get(sessionID)
	if err != nil {
		return ParentContext{}, err
	}
	if !exists || preference.Product != peer.Entrypoint {
		return ParentContext{}, errors.New("parent session preferences do not match its live registration")
	}
	return ParentContext{
		HostID: a.options.HostID, SessionID: sessionID, Product: preference.Product,
		InstanceID: peer.InstanceID, Groups: groups,
		AlwaysApprove: preference.AlwaysApprove,
		AdapterPID:    peer.PID, AdapterProcStart: peer.ProcStart, AdapterSocket: peer.Socket,
		PID: peer.LifecyclePID, ProcStart: peer.LifecycleProcStart,
		PermissionMode: peer.PermissionMode,
	}, nil
}

//nolint:gocyclo // Reconciliation keeps each identity and retirement transition explicit.
func (a *agent) reconcileRegisteredPeers() {
	logger := defaultLogger(a.logger)
	a.mu.Lock()
	changed := false
	if a.retirements == nil {
		a.retirements = map[string]localPeer{}
	}
	for id, peer := range a.local {
		adapterStatus := exactProcessStatus(peerAdapterRoot(peer))
		lifecycleStatus := exactProcessStatus(exactProcess{
			PID: peer.LifecyclePID, Start: peer.LifecycleProcStart, StrongStart: peer.LifecycleStrongStart,
		})
		if lifecycleStatus == procinfo.Unknown || adapterStatus == procinfo.Unknown {
			continue
		}
		if lifecycleStatus != procinfo.Known && peer.PID != peer.LifecyclePID {
			a.retirements[id] = peer
		}
		if adapterStatus != procinfo.Known || lifecycleStatus != procinfo.Known || !probeUnix(peer.Socket, 50*time.Millisecond) {
			delete(a.local, id)
			changed = true
		}
	}
	retirements := make(map[string]localPeer, len(a.retirements))
	for id, peer := range a.retirements {
		retirements[id] = peer
	}
	a.mu.Unlock()
	if changed {
		a.signalLocalChanged()
	}
	for id, peer := range retirements {
		if err := retirePeerAdapter(peer); err != nil {
			logger.Printf("clean peer adapter artifacts %s failed: %v", id, err)
			continue
		}
		a.mu.Lock()
		current, exists := a.retirements[id]
		if exists && current.PID == peer.PID && current.ProcStart == peer.ProcStart {
			delete(a.retirements, id)
		}
		a.mu.Unlock()
	}
}

func (a *agent) signalLocalChanged() {
	select {
	case a.localChanged <- struct{}{}:
	default:
	}
}
