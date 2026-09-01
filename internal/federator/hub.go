package federator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// HubOptions configures the central roster and routing service.
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

type deliveryRoute struct {
	source, destination *hubClient
	sourceID, targetID  string
}

type hubClient struct {
	hostID       string
	hostName     string
	wire         *wireConn
	peers        map[string]Peer
	capabilities []string
}

// RunHub serves federation agents until ctx is canceled.
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
	hub := &hub{
		logger: defaultLogger(options.Logger), clients: map[string]*hubClient{}, laneRoutes: map[string]*laneRoute{},
		clientTimeout: options.ClientTimeout,
	}
	hub.logger.Printf("hub listening on %s", listener.Addr())
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		hub.closeClients()
	}()
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return acceptErr
		}
		go hub.handleConnection(conn)
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
	clientTimeout := h.clientTimeout
	if clientTimeout <= 0 {
		clientTimeout = 20 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(clientTimeout))
	var client *hubClient
	err := scanMessages(conn, func(message Message) error {
		_ = conn.SetReadDeadline(time.Now().Add(clientTimeout))
		if client == nil {
			if message.Type == "probe" {
				if message.Version != ProtocolVersion {
					return errors.New("incompatible probe protocol version")
				}
				return newWireConn(conn).Send(Message{Type: "probe_ok", Version: ProtocolVersion})
			}
			if err := validateHello(message); err != nil {
				return err
			}
			client = &hubClient{
				hostID: message.HostID, hostName: message.HostName,
				wire: newWireConn(conn), peers: map[string]Peer{}, capabilities: normalizeCapabilities(message.Capabilities),
			}
			h.register(client)
			return client.wire.Send(Message{Type: "hello_ok", Version: ProtocolVersion})
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
		_ = previous.wire.conn.Close()
	}
	h.logger.Printf("host %s (%s) connected", client.hostName, client.hostID)
	h.broadcastRoster()
}

func (h *hub) unregister(client *hubClient) {
	h.mu.Lock()
	if h.clients[client.hostID] == client {
		delete(h.clients, client.hostID)
	}
	h.mu.Unlock()
	h.dropLaneRoutes(client)
	h.dropDeliveryRoutes(client)
	h.logger.Printf("host %s (%s) removed", client.hostName, client.hostID)
	h.broadcastRoster()
}

//nolint:gocyclo // Hub protocol variants are intentionally dispatched in one audited switch.
func (h *hub) handleClientMessage(client *hubClient, message Message) error {
	switch message.Type {
	case "snapshot":
		peers := make(map[string]Peer, len(message.Peers))
		sessions := make(map[string]bool, len(message.Peers))
		for _, peer := range message.Peers {
			if err := validateGroupedSnapshotPeer(peer, client.hostID); err != nil {
				return err
			}
			if _, exists := peers[peer.ID]; exists || sessions[peer.SessionID] {
				return errors.New("snapshot contains a duplicate peer identity")
			}
			peers[peer.ID] = peer
			sessions[peer.SessionID] = true
		}
		h.mu.Lock()
		if h.clients[client.hostID] == client {
			client.peers = peers
		}
		h.mu.Unlock()
		h.broadcastRoster()
		return nil
	case "deliver":
		return errors.New("legacy flat delivery is not supported by protocol v3")
	case "group_deliver":
		if err := h.routeGrouped(client, message); err != nil {
			h.logger.Printf("grouped delivery %s -> %s dropped: %v", message.SourceID, message.TargetID, err)
			return client.wire.Send(Message{
				Type: "delivery_error", SourceID: message.SourceID,
				TargetID: message.TargetID, RequestID: message.RequestID, Error: err.Error(),
			})
		}
		return nil
	case "terminal_notice_deliver":
		if err := h.routeTerminalNotice(client, message); err != nil {
			h.logger.Printf("terminal notice %s -> %s dropped: %v", message.SourceID, message.TargetID, err)
			return client.wire.Send(Message{
				Type: "delivery_error", SourceID: message.SourceID,
				TargetID: message.TargetID, RequestID: message.RequestID, Error: err.Error(),
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

func validateGroupedSnapshotPeer(peer Peer, hostID string) error {
	if peer.HostID != hostID || !validCatalogSessionID(peer.SessionID) ||
		peer.ID != hostID+"/"+peer.SessionID || peer.GlobalID != globalSessionID(hostID, peer.SessionID) {
		return errors.New("snapshot contains an invalid peer identity")
	}
	if peer.PeerProtocol != GroupProtocolVersion || !validProduct(peer.Entrypoint) ||
		strings.TrimSpace(peer.Name) == "" || strings.TrimSpace(peer.InstanceID) == "" {
		return errors.New("snapshot contains an incompatible grouped peer")
	}
	groups, err := normalizeEffectiveGroups(peer.Groups)
	if err != nil || len(groups) == 0 || !containsStringValue(groups, privateGroupPrefix+hostID+"/"+peer.SessionID) {
		return errors.New("snapshot contains invalid peer groups")
	}
	return nil
}

func (h *hub) routeGrouped(source *hubClient, message Message) error {
	if len(message.Frame) == 0 || message.SourceID == "" || message.TargetID == "" {
		return errors.New("group_deliver requires source_id, target_id, and frame")
	}
	h.mu.Lock()
	sourcePeer, sourceExists := source.peers[message.SourceID]
	var targetClient *hubClient
	var targetPeer Peer
	for _, candidate := range h.clients {
		if peer, exists := candidate.peers[message.TargetID]; exists {
			targetClient, targetPeer = candidate, peer
			break
		}
	}
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
	if err := validateCurrentDeliveryGroups(sourcePeer, targetPeer, frame); err != nil {
		return err
	}
	return h.forwardDelivery(source, targetClient, Message{
		Type: "group_deliver", SourceID: message.SourceID, TargetID: message.TargetID, Frame: message.Frame,
		RequestID: message.RequestID,
	})
}

//nolint:gocyclo // Durable-source and live-target attestations are kept explicit.
func (h *hub) routeTerminalNotice(source *hubClient, message Message) error {
	if len(message.Frame) == 0 || message.SourceID == "" || message.TargetID == "" {
		return errors.New("terminal_notice_deliver requires source_id, target_id, and frame")
	}
	var frame AgentFrame
	if json.Unmarshal(message.Frame, &frame) != nil || frame.Version != AgentFrameVersion || frame.Type != "delivery" ||
		frame.Source == nil || frame.Source.ID != message.SourceID || frame.SourceSessionID != frame.Source.SessionID {
		return errors.New("terminal notice contains an invalid agent frame")
	}
	if frame.Source.HostID != source.hostID {
		return errors.New("terminal notice source is not owned by the sending host")
	}
	if err := validateGroupedSnapshotPeer(*frame.Source, source.hostID); err != nil {
		return err
	}
	h.mu.Lock()
	var targetClient *hubClient
	var targetPeer Peer
	for _, candidate := range h.clients {
		if peer, exists := candidate.peers[message.TargetID]; exists {
			targetClient, targetPeer = candidate, peer
			break
		}
	}
	h.mu.Unlock()
	if targetClient == nil {
		return fmt.Errorf("target peer %s is not live", message.TargetID)
	}
	if err := validateCurrentDeliveryGroups(*frame.Source, targetPeer, frame); err != nil {
		return err
	}
	if targetPeer.HostID == source.hostID {
		return errors.New("terminal notice target must be on another host")
	}
	return h.forwardDelivery(source, targetClient, Message{
		Type: "terminal_notice_deliver", SourceID: message.SourceID, TargetID: message.TargetID, Frame: message.Frame,
		RequestID: message.RequestID,
	})
}

func (h *hub) forwardDelivery(source, destination *hubClient, message Message) error {
	if message.RequestID == "" {
		return errors.New("federated delivery requires request_id")
	}
	h.mu.Lock()
	if h.deliveryRoutes == nil {
		h.deliveryRoutes = map[string]*deliveryRoute{}
	}
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
		// An endpoint may disconnect after delivery but before its outcome. The
		// disconnect path already resolves or abandons that request; a late ACK
		// must not disconnect the surviving host.
		return nil
	}
	return route.source.wire.Send(message)
}

func (h *hub) dropDeliveryRoutes(client *hubClient) {
	type failedRoute struct {
		requestID string
		route     *deliveryRoute
	}
	var failed []failedRoute
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
				Type: "delivery_error", RequestID: item.requestID,
				SourceID: item.route.sourceID, TargetID: item.route.targetID,
				Error: "destination host disconnected before delivery acknowledgement",
			})
		}()
	}
}

func (h *hub) broadcastRoster() {
	h.broadcastMu.Lock()
	defer h.broadcastMu.Unlock()
	h.mu.Lock()
	clients := make([]*hubClient, 0, len(h.clients))
	peerCount := 0
	for _, client := range h.clients {
		peerCount += len(client.peers)
	}
	peers := make([]Peer, 0, peerCount)
	hosts := make([]Host, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
		hosts = append(hosts, Host{ID: client.hostID, Name: client.hostName, Capabilities: append([]string(nil), client.capabilities...)})
		for _, peer := range client.peers {
			peers = append(peers, peer)
		}
	}
	h.mu.Unlock()
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	message := Message{Type: "roster", Version: ProtocolVersion, Peers: peers, Hosts: hosts}
	var wait sync.WaitGroup
	for _, client := range clients {
		wait.Add(1)
		go func(client *hubClient) {
			defer wait.Done()
			if err := client.wire.Send(message); err != nil {
				_ = client.wire.conn.Close()
			}
		}(client)
	}
	wait.Wait()
}

func clientHost(client *hubClient) string {
	if client == nil {
		return ""
	}
	return client.hostID
}
