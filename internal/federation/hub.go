package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"
)

const maxHubLaneResponses = 256

// HubOptions configures the independent central federation runtime.
type HubOptions struct {
	Listen        string
	ClientTimeout time.Duration
	Logger        *log.Logger
}

type hub struct {
	logger         *log.Logger
	mu             sync.Mutex
	broadcastMu    sync.Mutex
	clients        map[string]*hubClient
	laneRoutes     map[string]*laneRoute
	deliveryRoutes map[string]*deliveryRoute
	clientTimeout  time.Duration
}

type hubClient struct {
	hostID       string
	hostName     string
	build        string
	wire         *wireConn
	peers        map[string]Peer
	capabilities []string
}

type deliveryRoute struct {
	source, destination *hubClient
	sourceID, targetID  string
}

type laneRoute struct {
	source, destination  *hubClient
	sourceID, targetHost string
	responses            chan Message
	done                 chan struct{}
	stopOnce             sync.Once
}

// RunHub serves protocol-compatible federation hosts until ctx is canceled.
func RunHub(ctx context.Context, options HubOptions) error {
	if options.Listen == "" {
		return errors.New("hub listen address is required")
	}
	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", options.Listen, err)
	}
	defer func() { _ = listener.Close() }()
	if options.ClientTimeout <= 0 {
		options.ClientTimeout = 20 * time.Second
	}
	h := &hub{
		logger: defaultLogger(options.Logger), clients: map[string]*hubClient{},
		laneRoutes: map[string]*laneRoute{}, deliveryRoutes: map[string]*deliveryRoute{},
		clientTimeout: options.ClientTimeout,
	}
	h.logger.Printf("hub listening on %s", listener.Addr())
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		h.closeClients()
	}()
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return acceptErr
		}
		go h.handleConnection(conn)
	}
}

func (h *hub) closeClients() {
	h.mu.Lock()
	clients := make([]*hubClient, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()
	for _, client := range clients {
		_ = client.wire.conn.Close()
	}
}

func (h *hub) handleConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	timeout := h.clientTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var client *hubClient
	err := scanMessages(conn, func(message Message) error {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		if client == nil {
			if message.Type == "probe" {
				if message.Version != ProtocolVersion {
					return errors.New("incompatible probe protocol version")
				}
				return newWireConn(conn).Send(Message{Type: "probe_ok", Version: ProtocolVersion, Build: RuntimeVersion})
			}
			if err := validateHello(message); err != nil {
				return err
			}
			capabilities, err := normalizeCapabilities(message.Capabilities)
			if err != nil {
				return err
			}
			candidate := &hubClient{
				hostID: message.HostID, hostName: message.HostName, build: message.Build,
				wire: newWireConn(conn), peers: map[string]Peer{}, capabilities: capabilities,
			}
			client = candidate
			if err := client.wire.Send(Message{Type: "hello_ok", Version: ProtocolVersion, Build: RuntimeVersion}); err != nil {
				return err
			}
			h.register(client)
			return nil
		}
		return h.handleClientMessage(client, message)
	})
	if client != nil {
		h.unregister(client)
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		h.logger.Printf("host %s disconnected: %v", defaultString(clientHost(client), conn.RemoteAddr().String()), err)
	}
}

func (h *hub) register(client *hubClient) {
	h.mu.Lock()
	previous := h.clients[client.hostID]
	h.clients[client.hostID] = client
	h.mu.Unlock()
	if previous != nil && previous != client {
		h.dropLaneRoutes(previous)
		h.dropDeliveryRoutes(previous)
		_ = previous.wire.conn.Close()
	}
	h.logger.Printf("host %s (%s) build %s connected", client.hostName, client.hostID, client.build)
	go h.broadcastRoster()
}

func (h *hub) unregister(client *hubClient) {
	h.mu.Lock()
	if h.clients[client.hostID] != client {
		h.mu.Unlock()
		return
	}
	delete(h.clients, client.hostID)
	h.mu.Unlock()
	h.dropLaneRoutes(client)
	h.dropDeliveryRoutes(client)
	h.logger.Printf("host %s (%s) removed", client.hostName, client.hostID)
	h.broadcastRoster()
}

//nolint:gocyclo // Hub protocol variants remain a closed audited switch.
func (h *hub) handleClientMessage(client *hubClient, message Message) error {
	h.mu.Lock()
	current := h.clients[client.hostID] == client
	h.mu.Unlock()
	if !current {
		return errors.New("host connection was replaced")
	}
	switch message.Type {
	case "snapshot":
		peers, err := validateSnapshot(message, client.hostID)
		if err != nil {
			return err
		}
		h.mu.Lock()
		if h.clients[client.hostID] != client {
			h.mu.Unlock()
			return errors.New("host connection was replaced")
		}
		client.peers = peers
		h.mu.Unlock()
		h.broadcastRoster()
		return nil
	case "group_deliver":
		if err := h.routeGrouped(client, message); err != nil {
			return client.wire.Send(Message{
				Type: "delivery_error", SourceID: message.SourceID, TargetID: message.TargetID,
				RequestID: message.RequestID, Error: err.Error(),
			})
		}
		return nil
	case "terminal_notice_deliver":
		if err := h.routeTerminalNotice(client, message); err != nil {
			return client.wire.Send(Message{
				Type: "delivery_error", SourceID: message.SourceID, TargetID: message.TargetID,
				RequestID: message.RequestID, Error: err.Error(),
			})
		}
		return nil
	case "delivery_ack", "delivery_error":
		return h.routeDeliveryOutcome(client, message)
	case "ping":
		return client.wire.Send(Message{Type: "pong"})
	case "lane_exec":
		return h.routeLaneExec(client, message)
	case "lane_cancel":
		return h.routeLaneCancel(client, message)
	case "lane_stdout", "lane_stderr", "lane_exit", "lane_error":
		return h.routeLaneResponse(client, message)
	default:
		return fmt.Errorf("unsupported client frame %q", message.Type)
	}
}

func validateSnapshot(message Message, hostID string) (map[string]Peer, error) {
	peers := make(map[string]Peer, len(message.Peers))
	sessions := make(map[string]bool, len(message.Peers))
	for _, peer := range message.Peers {
		if err := validateWirePeer(peer, hostID); err != nil {
			return nil, err
		}
		if _, exists := peers[peer.ID]; exists || sessions[peer.SessionID] {
			return nil, errors.New("snapshot contains a duplicate peer identity")
		}
		peers[peer.ID], sessions[peer.SessionID] = clonePeer(peer), true
	}
	return peers, nil
}

func (h *hub) routeGrouped(source *hubClient, message Message) error {
	if len(message.Frame) == 0 || message.SourceID == "" || message.TargetID == "" || message.RequestID == "" {
		return errors.New("group_deliver requires request_id, source_id, target_id, and frame")
	}
	h.mu.Lock()
	sourcePeer, sourceExists := source.peers[message.SourceID]
	targetClient, targetPeer := h.findPeerLocked(message.TargetID)
	h.mu.Unlock()
	if !sourceExists {
		return fmt.Errorf("source peer %s is not advertised by host %s", message.SourceID, source.hostID)
	}
	if targetClient == nil {
		return fmt.Errorf("target peer %s is not live", message.TargetID)
	}
	var frame AgentFrame
	if json.Unmarshal(message.Frame, &frame) != nil || frame.Version != AgentFrameVersion || frame.Type != "delivery" ||
		frame.Source == nil || frame.Source.ID != sourcePeer.ID || frame.SourceSessionID != sourcePeer.SessionID {
		return errors.New("group_deliver contains an invalid agent frame")
	}
	if err := validateDeliveryGroups(sourcePeer, targetPeer, frame); err != nil {
		return err
	}
	return h.forwardDelivery(source, targetClient, message)
}

func (h *hub) routeTerminalNotice(source *hubClient, message Message) error {
	frame, err := validateTerminalNotice(source, message)
	if err != nil {
		return err
	}
	h.mu.Lock()
	targetClient, targetPeer := h.findPeerLocked(message.TargetID)
	h.mu.Unlock()
	if targetClient == nil {
		return fmt.Errorf("target peer %s is not live", message.TargetID)
	}
	if targetPeer.HostID == source.hostID {
		return errors.New("terminal notice target must be on another host")
	}
	if err := validateDeliveryGroups(*frame.Source, targetPeer, frame); err != nil {
		return err
	}
	return h.forwardDelivery(source, targetClient, message)
}

func validateTerminalNotice(source *hubClient, message Message) (AgentFrame, error) {
	if len(message.Frame) == 0 || message.SourceID == "" || message.TargetID == "" || message.RequestID == "" {
		return AgentFrame{}, errors.New("terminal_notice_deliver requires request_id, source_id, target_id, and frame")
	}
	var frame AgentFrame
	if json.Unmarshal(message.Frame, &frame) != nil || frame.Version != AgentFrameVersion || frame.Type != "delivery" ||
		frame.Source == nil || frame.Source.ID != message.SourceID || frame.SourceSessionID != frame.Source.SessionID {
		return AgentFrame{}, errors.New("terminal notice contains an invalid agent frame")
	}
	if frame.Source.HostID != source.hostID || validateWirePeer(*frame.Source, source.hostID) != nil {
		return AgentFrame{}, errors.New("terminal notice source is not owned by the sending host")
	}
	return frame, nil
}

func (h *hub) findPeerLocked(id string) (*hubClient, Peer) {
	for _, candidate := range h.clients {
		if peer, exists := candidate.peers[id]; exists {
			return candidate, peer
		}
	}
	return nil, Peer{}
}

func (h *hub) forwardDelivery(source, destination *hubClient, message Message) error {
	h.mu.Lock()
	if _, exists := h.deliveryRoutes[message.RequestID]; exists {
		h.mu.Unlock()
		return errors.New("duplicate federated delivery request")
	}
	h.deliveryRoutes[message.RequestID] = &deliveryRoute{
		source: source, destination: destination, sourceID: message.SourceID, targetID: message.TargetID,
	}
	h.mu.Unlock()
	if err := destination.wire.Send(message); err != nil {
		h.mu.Lock()
		delete(h.deliveryRoutes, message.RequestID)
		h.mu.Unlock()
		return err
	}
	return nil
}

func (h *hub) routeDeliveryOutcome(destination *hubClient, message Message) error {
	h.mu.Lock()
	route := h.deliveryRoutes[message.RequestID]
	if route != nil && route.destination == destination {
		delete(h.deliveryRoutes, message.RequestID)
	}
	h.mu.Unlock()
	if route == nil || route.destination != destination || message.SourceID != route.sourceID || message.TargetID != route.targetID {
		return nil
	}
	return route.source.wire.Send(message)
}

func (h *hub) dropDeliveryRoutes(client *hubClient) {
	type failedRoute struct {
		requestID string
		route     *deliveryRoute
	}
	failed := []failedRoute{}
	h.mu.Lock()
	for requestID, route := range h.deliveryRoutes {
		if route.source != client && route.destination != client {
			continue
		}
		delete(h.deliveryRoutes, requestID)
		if route.destination == client && route.source != client {
			failed = append(failed, failedRoute{requestID: requestID, route: route})
		}
	}
	h.mu.Unlock()
	for _, item := range failed {
		item := item
		go func() {
			_ = item.route.source.wire.Send(Message{
				Type: "delivery_error", RequestID: item.requestID, SourceID: item.route.sourceID,
				TargetID: item.route.targetID, Error: "destination host disconnected before delivery acknowledgement",
			})
		}()
	}
}

func newLaneRoute(source, destination *hubClient, sourceID, targetHost string) *laneRoute {
	return &laneRoute{
		source: source, destination: destination, sourceID: sourceID, targetHost: targetHost,
		responses: make(chan Message, maxHubLaneResponses), done: make(chan struct{}),
	}
}

func (r *laneRoute) stop() { r.stopOnce.Do(func() { close(r.done) }) }

func (h *hub) routeLaneExec(source *hubClient, message Message) error {
	capability, capabilityErr := laneCapabilityForMessage(message)
	if capabilityErr != nil {
		return sendLaneRouteError(source, message.RequestID, capabilityErr.Error())
	}
	if reason := validateLaneExecRequest(message, capability); reason != "" {
		return sendLaneRouteError(source, message.RequestID, reason)
	}
	destination, route, reason := h.admitLaneRoute(source, message, capability)
	if reason != "" {
		return sendLaneRouteError(source, message.RequestID, reason)
	}
	go h.forwardLaneRoute(message.RequestID, route)
	if err := destination.wire.Send(message); err != nil {
		h.removeLaneRoute(message.RequestID, route)
		return sendLaneRouteError(source, message.RequestID, "forward remote lane request: "+err.Error())
	}
	return nil
}

func validateLaneExecRequest(message Message, capability string) string {
	if message.RequestID == "" || message.SourceID == "" || message.TargetHostID == "" || capability == "" || len(message.Args) == 0 {
		return "invalid remote lane request"
	}
	if len(message.Input) > maxLaneInputBytes || validateRemoteLaneArgs(message.Args) != nil {
		return "remote lane request exceeds protocol bounds"
	}
	return ""
}

func (h *hub) admitLaneRoute(
	source *hubClient,
	message Message,
	capability string,
) (*hubClient, *laneRoute, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sourcePeer, sourceExists := source.peers[message.SourceID]
	parentValid := sourceExists && message.ParentContext != nil && parentMatchesPeer(*message.ParentContext, sourcePeer)
	destination := h.clients[message.TargetHostID]
	_, duplicate := h.laneRoutes[message.RequestID]
	switch {
	case !sourceExists:
		return nil, nil, "source peer is not advertised by this host"
	case destination == nil:
		return nil, nil, "target host is not connected to the hub"
	case duplicate:
		return nil, nil, "duplicate lane request id"
	case !contains(destination.capabilities, capability):
		return nil, nil, "target host lacks the requested lane capability"
	case !parentValid:
		return nil, nil, "remote lane parent context does not match its source peer"
	}
	route := newLaneRoute(source, destination, message.SourceID, message.TargetHostID)
	h.laneRoutes[message.RequestID] = route
	return destination, route, ""
}

func sendLaneRouteError(source *hubClient, requestID, reason string) error {
	return source.wire.Send(Message{Type: "lane_error", RequestID: requestID, Error: reason})
}

func (h *hub) routeLaneCancel(source *hubClient, message Message) error {
	h.mu.Lock()
	route := h.laneRoutes[message.RequestID]
	h.mu.Unlock()
	if route == nil || route.source != source {
		return nil
	}
	if err := route.destination.wire.Send(Message{Type: "lane_cancel", RequestID: message.RequestID}); err != nil {
		h.removeLaneRoute(message.RequestID, route)
		return source.wire.Send(Message{Type: "lane_error", RequestID: message.RequestID, Error: "target host disconnected while cancelling remote lane"})
	}
	return nil
}

func (h *hub) routeLaneResponse(destination *hubClient, message Message) error {
	h.mu.Lock()
	route := h.laneRoutes[message.RequestID]
	h.mu.Unlock()
	if route == nil || route.destination != destination {
		return nil
	}
	select {
	case route.responses <- message:
		return nil
	default:
		if h.removeLaneRoute(message.RequestID, route) {
			go h.failLaneRoute(message.RequestID, route, "remote lane output exceeded the hub route buffer")
		}
		return nil
	}
}

func (h *hub) forwardLaneRoute(requestID string, route *laneRoute) {
	for {
		select {
		case <-route.done:
			return
		case message := <-route.responses:
			terminal := message.Type == "lane_exit" || message.Type == "lane_error"
			if terminal && !h.removeLaneRoute(requestID, route) {
				return
			}
			if err := route.source.wire.Send(message); err != nil {
				if !terminal && h.removeLaneRoute(requestID, route) {
					_ = route.destination.wire.Send(Message{Type: "lane_cancel", RequestID: requestID})
				}
				return
			}
			if terminal {
				return
			}
		}
	}
}

func (h *hub) removeLaneRoute(requestID string, route *laneRoute) bool {
	h.mu.Lock()
	if h.laneRoutes[requestID] != route {
		h.mu.Unlock()
		return false
	}
	delete(h.laneRoutes, requestID)
	h.mu.Unlock()
	route.stop()
	return true
}

func (h *hub) failLaneRoute(requestID string, route *laneRoute, reason string) {
	_ = route.destination.wire.Send(Message{Type: "lane_cancel", RequestID: requestID})
	h.removeLaneRoute(requestID, route)
	_ = route.source.wire.Send(Message{Type: "lane_error", RequestID: requestID, Error: reason})
}

func (h *hub) dropLaneRoutes(client *hubClient) {
	type notification struct {
		wire    *wireConn
		message Message
	}
	notifications := []notification{}
	h.mu.Lock()
	for requestID, route := range h.laneRoutes {
		switch client {
		case route.source:
			notifications = append(notifications, notification{wire: route.destination.wire, message: Message{Type: "lane_cancel", RequestID: requestID}})
		case route.destination:
			notifications = append(notifications, notification{wire: route.source.wire, message: Message{Type: "lane_error", RequestID: requestID, Error: "target host disconnected from hub"}})
		default:
			continue
		}
		delete(h.laneRoutes, requestID)
		route.stop()
	}
	h.mu.Unlock()
	for _, current := range notifications {
		current := current
		go func() { _ = current.wire.Send(current.message) }()
	}
}

func (h *hub) broadcastRoster() {
	h.broadcastMu.Lock()
	defer h.broadcastMu.Unlock()
	h.mu.Lock()
	clients, roster := h.uniformRosterLocked()
	h.mu.Unlock()
	var wait sync.WaitGroup
	for _, client := range clients {
		client := client
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := client.wire.Send(roster); err != nil {
				_ = client.wire.conn.Close()
			}
		}()
	}
	wait.Wait()
}

// uniformRosterLocked computes the one complete roster delivered to every client.
func (h *hub) uniformRosterLocked() ([]*hubClient, Message) {
	clients := make([]*hubClient, 0, len(h.clients))
	peers := []Peer{}
	hosts := make([]Host, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
		hosts = append(hosts, Host{
			ID: client.hostID, Name: client.hostName, Capabilities: append([]string(nil), client.capabilities...),
			Build: client.build,
		})
		for _, peer := range client.peers {
			peers = append(peers, clonePeer(peer))
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	return clients, Message{Type: "roster", Version: ProtocolVersion, Hosts: hosts, Peers: peers}
}

func clientHost(client *hubClient) string {
	if client == nil {
		return ""
	}
	return client.hostID
}
