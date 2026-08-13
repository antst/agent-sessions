package federator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
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
	logger        *log.Logger
	mu            sync.Mutex
	broadcastMu   sync.Mutex
	clients       map[string]*hubClient
	laneRoutes    map[string]*laneRoute
	clientTimeout time.Duration
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
	h.logger.Printf("host %s (%s) removed", client.hostName, client.hostID)
	h.broadcastRoster()
}

func (h *hub) handleClientMessage(client *hubClient, message Message) error {
	switch message.Type {
	case "snapshot":
		peers := make(map[string]Peer, len(message.Peers))
		for _, peer := range message.Peers {
			if peer.HostID != client.hostID || peer.ID == "" || peer.SessionID == "" {
				return errors.New("snapshot contains a peer outside the sending host")
			}
			peers[peer.ID] = peer
		}
		h.mu.Lock()
		if h.clients[client.hostID] == client {
			client.peers = peers
		}
		h.mu.Unlock()
		h.broadcastRoster()
		return nil
	case "deliver":
		if err := h.route(client, message); err != nil {
			h.logger.Printf("delivery %s -> %s dropped: %v", message.SourceID, message.TargetID, err)
			return client.wire.Send(Message{
				Type: "delivery_error", SourceID: message.SourceID,
				TargetID: message.TargetID, Error: err.Error(),
			})
		}
		return nil
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

func (h *hub) route(source *hubClient, message Message) error {
	if len(message.Frame) == 0 || message.SourceID == "" || message.TargetID == "" {
		return errors.New("deliver requires source_id, target_id, and frame")
	}
	h.mu.Lock()
	_, sourceExists := source.peers[message.SourceID]
	var target *hubClient
	for _, candidate := range h.clients {
		if _, exists := candidate.peers[message.TargetID]; exists {
			target = candidate
			break
		}
	}
	h.mu.Unlock()
	if !sourceExists {
		return fmt.Errorf("source peer %s is not advertised by host %s", message.SourceID, source.hostID)
	}
	if target == nil {
		return fmt.Errorf("target peer %s is not live", message.TargetID)
	}
	return target.wire.Send(Message{
		Type: "deliver", SourceID: message.SourceID, TargetID: message.TargetID, Frame: message.Frame,
	})
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
