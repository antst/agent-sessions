package federation

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/diagnostics"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const (
	defaultHubClientTimeout = 20 * time.Second
	defaultHubWriteTimeout  = 10 * time.Second
)

// HubOptions configure only the separately deployed central listener.
type HubOptions struct {
	Listen            string
	RuntimeVersion    string
	RuntimeIdentity   string
	ServiceManager    string
	ServiceUnit       string
	ClientTimeout     time.Duration
	ResourcePreflight func(context.Context, HubAdmissionRequest) error
	Stdout            io.Writer
	Stderr            io.Writer
}

type hubWireFrame struct {
	Type            string              `json:"type"`
	Version         int                 `json:"version,omitempty"`
	HostID          string              `json:"host_id,omitempty"`
	HostName        string              `json:"host_name,omitempty"`
	RuntimeVersion  string              `json:"runtime_version,omitempty"`
	RuntimeIdentity string              `json:"runtime_identity,omitempty"`
	Generation      uint64              `json:"generation,omitempty"`
	Products        []string            `json:"products,omitempty"`
	Capabilities    []string            `json:"capabilities,omitempty"`
	Hosts           []Host              `json:"hosts,omitempty"`
	Peers           []Peer              `json:"peers,omitempty"`
	SourceID        string              `json:"source_id,omitempty"`
	TargetID        string              `json:"target_id,omitempty"`
	TargetHostID    string              `json:"target_host_id,omitempty"`
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

type hubWireConnection struct {
	connection net.Conn
	mu         sync.Mutex
}

func (wire *hubWireConnection) send(message hubWireFrame) error {
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
	if err := wire.connection.SetWriteDeadline(time.Now().Add(defaultHubWriteTimeout)); err != nil {
		return err
	}
	_, err = wire.connection.Write(body)
	return err
}

type hubRuntimeClient struct {
	hostID string
	owner  uint64
	wire   *hubWireConnection
}

type hubRuntime struct {
	options  HubOptions
	paths    HubPaths
	registry *HubRegistry
	routes   *DeliveryRouteTable
	lanes    *RemoteLaneRouteTable
	archives *RemoteLaneArchiveRouteTable

	mu            sync.RWMutex
	clients       map[string]*hubRuntimeClient
	clientWG      sync.WaitGroup
	status        sync.Mutex
	observability *hubDiagnosticWriter
}

// RunHub owns one central listener until ctx is canceled. It imports only the
// logical federation registry/routing contracts and never creates host or
// vendor runtime authority.
func RunHub(ctx context.Context, options HubOptions) error {
	if err := validateHubListen(options.Listen); err != nil {
		return err
	}
	if options.RuntimeVersion == "" || options.RuntimeIdentity == "" {
		return errors.New("hub runtime requires exact version and content identity")
	}
	if options.ClientTimeout <= 0 {
		options.ClientTimeout = defaultHubClientTimeout
	}
	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", options.Listen, err)
	}
	defer func() { _ = listener.Close() }()
	paths, err := ResolveHubPaths()
	if err != nil {
		return err
	}
	prior, priorErr := ReadHubStatus(ctx)
	recoveredCrash := priorErr == nil && prior.PID > 0 && !hubStatusProcessMatches(prior)
	runtime := &hubRuntime{
		options: options, paths: paths, registry: NewHubRegistry(), routes: &DeliveryRouteTable{},
		lanes: &RemoteLaneRouteTable{}, archives: &RemoteLaneArchiveRouteTable{},
		clients: make(map[string]*hubRuntimeClient), observability: newHubDiagnosticWriter(options.Stdout, options.Stderr),
	}
	runtime.options.Listen = listener.Addr().String()
	if err := runtime.publishStatus(ctx); err != nil {
		return fmt.Errorf("publish hub readiness: %w", err)
	}
	runtime.emitStarted(recoveredCrash)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		runtime.closeClients()
	}()
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				runtime.clientWG.Wait()
				runtime.observability.emit(diagnostics.OutputNormal, "hub.stopping", map[string]any{
					"role": "hub", "state": "stopping", "listener": runtime.options.Listen,
				})
				_ = runtime.publishStopped(context.Background())
				return nil
			}
			return acceptErr
		}
		runtime.clientWG.Add(1)
		go func() {
			defer runtime.clientWG.Done()
			runtime.handleConnection(ctx, connection)
		}()
	}
}

func (runtime *hubRuntime) handleConnection(ctx context.Context, connection net.Conn) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetReadDeadline(time.Now().Add(runtime.options.ClientTimeout))
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	wire := &hubWireConnection{connection: connection}
	first, err := scanHubWireFrame(scanner)
	if err != nil {
		return
	}
	if first.Type == "probe" {
		if ValidateProtocolHandshake(first.Version) == nil {
			_ = wire.send(hubWireFrame{Type: "probe_ok", Version: ProtocolVersion})
		}
		return
	}
	if first.Type != "hello" {
		_ = wire.send(hubWireFrame{Type: "error", Version: ProtocolVersion, Error: "first federation frame must be hello"})
		return
	}
	advertisement := HostAdvertisement{
		HostID: first.HostID, HostName: first.HostName, ProtocolVersion: first.Version,
		RuntimeVersion: first.RuntimeVersion, RuntimeIdentity: first.RuntimeIdentity,
		Generation: first.Generation, Products: first.Products, Capabilities: first.Capabilities,
	}
	preflight := runtime.options.ResourcePreflight
	if preflight == nil {
		preflight = func(context.Context, HubAdmissionRequest) error { return nil }
	}
	requestIdentity := fmt.Sprintf("register-%x", sha256.Sum256([]byte(first.HostID+"\x00"+fmt.Sprint(first.Generation))))
	admission, err := AdmitHubWork(ctx, HubAdmissionRequest{
		Operation: "host.register", HostID: first.HostID, RequestID: requestIdentity,
	}, HubAdmissionHooks{
		Preflight: preflight,
		Commit: func(context.Context, HubAdmissionRequest) (uint64, error) {
			return runtime.registry.RegisterHost(advertisement)
		},
	})
	if err != nil {
		runtime.observability.emit(diagnostics.OutputError, "hub.admission", hubResourceFailureFields(err))
		_ = wire.send(hubWireFrame{Type: "error", Version: ProtocolVersion, Error: boundedHubError(err)})
		return
	}
	owner := admission.Revision
	client := &hubRuntimeClient{hostID: first.HostID, owner: owner, wire: wire}
	previous := runtime.replaceClient(client)
	if previous != nil {
		_ = previous.wire.connection.Close()
	}
	defer runtime.unregister(client)
	if err := wire.send(hubWireFrame{Type: "hello_ok", Version: ProtocolVersion}); err != nil {
		return
	}
	_ = connection.SetReadDeadline(time.Time{})
	runtime.broadcastRoster()
	for scanner.Scan() {
		var message hubWireFrame
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return
		}
		if err := runtime.handleFrame(ctx, client, message); err != nil {
			_ = wire.send(hubWireFrame{Type: "error", Version: ProtocolVersion, RequestID: message.RequestID, Error: boundedHubError(err)})
			return
		}
	}
}

func scanHubWireFrame(scanner *bufio.Scanner) (hubWireFrame, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return hubWireFrame{}, err
		}
		return hubWireFrame{}, io.EOF
	}
	var frame hubWireFrame
	if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
		return hubWireFrame{}, err
	}
	return frame, nil
}

func (runtime *hubRuntime) handleFrame(ctx context.Context, client *hubRuntimeClient, message hubWireFrame) error {
	switch message.Type {
	case "snapshot":
		if err := runtime.registry.ReplaceHostSnapshot(client.hostID, client.owner, message.Peers); err != nil {
			return err
		}
		runtime.broadcastRoster()
		return runtime.publishStatus(ctx)
	case "group_deliver", "terminal_notice_deliver":
		return runtime.forwardDelivery(message, client)
	case "delivery_ack", "delivery_error":
		return runtime.forwardDeliveryOutcome(message, client)
	case "ping":
		return client.wire.send(hubWireFrame{Type: "pong"})
	case "lane_exec":
		return runtime.forwardRemoteLane(message, client)
	case "lane_cancel":
		return runtime.forwardRemoteLaneCancel(message, client)
	case "lane_archive":
		return runtime.forwardRemoteLaneArchive(message, client)
	case "lane_accepted", "lane_cancelled", "lane_cancel_refused", "lane_archived", "lane_archive_refused", "lane_result", "lane_result_ack", "lane_result_refused", "lane_stdout", "lane_stderr", "lane_exit", "lane_error":
		return runtime.forwardRemoteLaneOutcome(message, client)
	case "deliver":
		return errors.New("legacy flat delivery is not supported by protocol v3")
	default:
		return fmt.Errorf("unsupported federation frame %q", message.Type)
	}
}

func (runtime *hubRuntime) forwardDelivery(message hubWireFrame, source *hubRuntimeClient) error {
	decision, err := ResolveHubDelivery(runtime.registry.Snapshot(), HubDeliveryRequest{
		Type: message.Type, RequestID: message.RequestID, SourceHostID: source.hostID,
		SourceID: message.SourceID, TargetID: message.TargetID, Frame: message.Frame,
	})
	if err != nil {
		return source.wire.send(hubWireFrame{
			Type: "delivery_error", RequestID: message.RequestID, SourceID: message.SourceID,
			TargetID: message.TargetID, Error: boundedHubError(err),
		})
	}
	remoteLaneRequestID := ""
	if decision.Type == "terminal_notice_deliver" {
		laneRoute, ok := runtime.lanes.AuthorizeNotice(
			decision.SourceHostID, decision.Source.SessionID, decision.TargetHostID, decision.Target.SessionID,
		)
		if !ok {
			return source.wire.send(hubWireFrame{
				Type: "delivery_error", RequestID: message.RequestID, SourceID: message.SourceID,
				TargetID: message.TargetID, Error: "terminal notice does not match an accepted remote lane",
			})
		}
		remoteLaneRequestID = laneRoute.RequestID
	}
	route := DeliveryRoute{
		RequestID: decision.RequestID, SourceHostID: decision.SourceHostID, SourceID: decision.Source.ID,
		TargetHostID: decision.TargetHostID, TargetID: decision.Target.ID,
		RemoteLaneRequestID: remoteLaneRequestID,
	}
	if err := runtime.routes.Begin(route); err != nil {
		return err
	}
	target := runtime.client(decision.TargetHostID)
	if target == nil {
		_, _ = runtime.routes.Resolve(route.RequestID, route.TargetHostID, route.SourceID, route.TargetID)
		return source.wire.send(hubWireFrame{
			Type: "delivery_error", RequestID: route.RequestID, SourceID: route.SourceID,
			TargetID: route.TargetID, Error: "destination host disconnected before delivery",
		})
	}
	if err := target.wire.send(hubWireFrame{
		Type: decision.Type, RequestID: decision.RequestID, SourceID: decision.Source.ID,
		TargetID: decision.Target.ID, Frame: decision.Frame,
	}); err != nil {
		_, _ = runtime.routes.Resolve(route.RequestID, route.TargetHostID, route.SourceID, route.TargetID)
		return err
	}
	return runtime.publishStatus(context.Background())
}

func (runtime *hubRuntime) forwardRemoteLane(message hubWireFrame, source *hubRuntimeClient) error {
	if message.RemoteLane == nil {
		return source.wire.send(hubWireFrame{
			Type: "lane_error", RequestID: message.RequestID,
			Error: "remote lane request does not use the typed lane envelope",
		})
	}
	envelope := cloneRemoteLaneEnvelope(*message.RemoteLane)
	if envelope.RequestID == "" {
		envelope.RequestID = message.RequestID
	}
	decision, err := runtime.lanes.Begin(runtime.registry.Snapshot(), source.hostID, envelope)
	if err != nil {
		return source.wire.send(hubWireFrame{Type: "lane_error", RequestID: envelope.RequestID, Error: boundedHubError(err)})
	}
	target := runtime.client(decision.Route.TargetHostID)
	if target == nil {
		// The same envelope may be reconstructing accepted work after a hub
		// restart. Absence of the destination connection cannot prove that the
		// native turn was never accepted, so preserve the route and let the
		// source retain durable uncertainty until a roster-driven replay.
		return nil
	}
	if target.wire.send(hubWireFrame{
		Type: "lane_exec", RequestID: decision.Route.RequestID, SourceID: decision.Route.SourceID,
		TargetHostID: decision.Route.TargetHostID, RemoteLane: &decision.Envelope,
	}) != nil {
		_ = target.wire.connection.Close()
		return runtime.publishStatus(context.Background())
	}
	return runtime.publishStatus(context.Background())
}

func (runtime *hubRuntime) forwardRemoteLaneCancel(message hubWireFrame, source *hubRuntimeClient) error {
	route, ok := runtime.lanes.Cancel(message.RequestID, source.hostID)
	if !ok {
		return errors.New("remote lane cancellation does not match an accepted source")
	}
	target := runtime.client(route.TargetHostID)
	if target == nil {
		// Cancellation remains pending until the exact destination accepts or
		// refuses it. A socket lifetime is not a terminal native decision.
		return nil
	}
	if err := target.wire.send(hubWireFrame{Type: "lane_cancel", RequestID: route.RequestID}); err != nil {
		_ = target.wire.connection.Close()
	}
	return nil
}

func (runtime *hubRuntime) forwardRemoteLaneArchive(message hubWireFrame, source *hubRuntimeClient) error {
	if message.RemoteArchive == nil {
		return source.wire.send(hubWireFrame{
			Type: "lane_archive_refused", RequestID: message.RequestID, Error: "remote lane archive identity is missing",
		})
	}
	request := *message.RemoteArchive
	if request.RequestID == "" {
		request.RequestID = message.RequestID
	}
	route, err := runtime.archives.Begin(runtime.registry.Snapshot(), source.hostID, request)
	if err != nil {
		return source.wire.send(hubWireFrame{Type: "lane_archive_refused", RequestID: request.RequestID, Error: boundedHubError(err)})
	}
	target := runtime.client(route.TargetHostID)
	if target == nil {
		_, _ = runtime.archives.Complete(route.RequestID, route.TargetHostID)
		return source.wire.send(hubWireFrame{
			Type: "lane_archive_refused", RequestID: request.RequestID, Error: "destination host is disconnected",
		})
	}
	request.SourceID = route.SourceID
	request.TargetHostID = route.TargetHostID
	if err := target.wire.send(hubWireFrame{
		Type: "lane_archive", RequestID: request.RequestID, SourceID: route.SourceID,
		TargetHostID: route.TargetHostID, RemoteArchive: &request,
	}); err != nil {
		_, _ = runtime.archives.Complete(route.RequestID, route.TargetHostID)
		_ = target.wire.connection.Close()
		return err
	}
	return nil
}

//nolint:gocyclo // Each authenticated lane wire family is resolved at one route-ownership boundary.
func (runtime *hubRuntime) forwardRemoteLaneOutcome(message hubWireFrame, target *hubRuntimeClient) error {
	if message.Type == "lane_archived" || message.Type == "lane_archive_refused" {
		route, ok := runtime.archives.Complete(message.RequestID, target.hostID)
		if !ok {
			return errors.New("remote lane archive result does not match its destination")
		}
		if message.Type == "lane_archived" && (message.RemoteArchived == nil ||
			message.RemoteArchived.RequestID != route.RequestID || message.RemoteArchived.LaneSessionID != route.LaneSessionID) {
			return errors.New("remote lane archive acknowledgement does not match its route")
		}
		source := runtime.client(route.SourceHostID)
		if source == nil {
			return nil
		}
		return source.wire.send(hubWireFrame{
			Type: message.Type, RequestID: route.RequestID, RemoteArchived: message.RemoteArchived, Error: message.Error,
		})
	}
	if message.Type == "lane_cancelled" || message.Type == "lane_cancel_refused" {
		route, ok := runtime.lanes.CancellationDecision(message.RequestID, target.hostID)
		if !ok {
			return errors.New("remote lane cancellation decision does not match an accepted destination")
		}
		source := runtime.client(route.SourceHostID)
		if source == nil {
			return nil
		}
		return source.wire.send(hubWireFrame{Type: message.Type, RequestID: route.RequestID, Error: message.Error})
	}
	if message.Type == "lane_result_ack" || message.Type == "lane_result_refused" {
		route, ok := runtime.lanes.ResultDecision(message.RequestID, target.hostID)
		if !ok {
			return errors.New("remote lane result decision does not match its accepted source")
		}
		destination := runtime.client(route.TargetHostID)
		if destination == nil {
			return nil
		}
		return destination.wire.send(hubWireFrame{Type: message.Type, RequestID: route.RequestID, Error: message.Error})
	}
	terminal := message.Type == "lane_exit" || message.Type == "lane_error"
	if message.Type == "lane_result" {
		if message.RemoteResult == nil || message.RemoteResult.RequestID != message.RequestID {
			return errors.New("remote lane result identity is incomplete")
		}
		if err := ValidateRemoteLaneInput(message.RemoteResult.ResultReference); err != nil {
			return err
		}
	}
	route, ok := runtime.lanes.Outcome(message.RequestID, target.hostID, terminal)
	if !ok {
		if message.Type == "lane_result" {
			return target.wire.send(hubWireFrame{
				Type: "lane_result_refused", RequestID: message.RequestID,
				Error: "remote lane result has no accepted route; retry after source recovery",
			})
		}
		return errors.New("remote lane outcome does not match an accepted destination")
	}
	if message.Type == "lane_result" && (message.RemoteResult.LaneSessionID != route.LaneSessionID ||
		message.RemoteResult.TurnID != route.TurnID || !remoteLaneTerminalOutcome(message.RemoteResult.Outcome)) {
		return errors.New("remote lane result does not match its accepted turn")
	}
	source := runtime.client(route.SourceHostID)
	if source == nil {
		return nil
	}
	if err := source.wire.send(hubWireFrame{
		Type: message.Type, RequestID: route.RequestID, Data: message.Data,
		ExitCode: message.ExitCode, Error: message.Error, RemoteAccepted: message.RemoteAccepted,
		RemoteResult: message.RemoteResult,
	}); err != nil {
		return err
	}
	return runtime.publishStatus(context.Background())
}

func (runtime *hubRuntime) forwardDeliveryOutcome(message hubWireFrame, destination *hubRuntimeClient) error {
	route, ok := runtime.routes.Resolve(message.RequestID, destination.hostID, message.SourceID, message.TargetID)
	if !ok {
		return nil
	}
	if message.Type == "delivery_ack" && route.RemoteLaneRequestID != "" &&
		!runtime.lanes.CompleteNotice(route.RemoteLaneRequestID, route.SourceHostID, route.TargetHostID) {
		return errors.New("terminal notice acknowledgement lost its remote lane ownership")
	}
	source := runtime.client(route.SourceHostID)
	if source == nil {
		return nil
	}
	if source.wire.send(message) != nil {
		_ = source.wire.connection.Close()
		return runtime.publishStatus(context.Background())
	}
	if message.Type == "delivery_ack" && route.RemoteLaneRequestID != "" &&
		!runtime.lanes.FinalizeNotice(route.RemoteLaneRequestID, route.SourceHostID, route.TargetHostID) {
		return errors.New("terminal notice acknowledgement lost its finalized remote lane ownership")
	}
	return runtime.publishStatus(context.Background())
}

func (runtime *hubRuntime) replaceClient(client *hubRuntimeClient) *hubRuntimeClient {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	previous := runtime.clients[client.hostID]
	runtime.clients[client.hostID] = client
	return previous
}

func (runtime *hubRuntime) client(hostID string) *hubRuntimeClient {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.clients[hostID]
}

func (runtime *hubRuntime) closeClients() {
	runtime.mu.RLock()
	clients := make([]*hubRuntimeClient, 0, len(runtime.clients))
	for _, client := range runtime.clients {
		clients = append(clients, client)
	}
	runtime.mu.RUnlock()
	for _, client := range clients {
		_ = client.wire.connection.Close()
	}
}

func (runtime *hubRuntime) unregister(client *hubRuntimeClient) {
	runtime.mu.Lock()
	if runtime.clients[client.hostID] != client {
		runtime.mu.Unlock()
		return
	}
	delete(runtime.clients, client.hostID)
	runtime.mu.Unlock()
	if !runtime.registry.UnregisterHost(client.hostID, client.owner) {
		return
	}
	runtime.archives.DropHost(client.hostID)
	for _, route := range runtime.routes.DropHost(client.hostID) {
		if source := runtime.client(route.SourceHostID); source != nil {
			_ = source.wire.send(hubWireFrame{
				Type: "delivery_error", RequestID: route.RequestID, SourceID: route.SourceID,
				TargetID: route.TargetID, Error: "destination host disconnected before delivery acknowledgement",
			})
		}
	}
	for _, route := range runtime.lanes.DropHost(client.hostID) {
		if source := runtime.client(route.SourceHostID); source != nil {
			_ = source.wire.send(hubWireFrame{
				Type: "lane_error", RequestID: route.RequestID,
				Error: "destination host disconnected before remote lane completion",
			})
		}
	}
	runtime.broadcastRoster()
	_ = runtime.publishStatus(context.Background())
}

func (runtime *hubRuntime) broadcastRoster() {
	snapshot := runtime.registry.Snapshot()
	runtime.mu.RLock()
	clients := make([]*hubRuntimeClient, 0, len(runtime.clients))
	for _, client := range runtime.clients {
		clients = append(clients, client)
	}
	runtime.mu.RUnlock()
	message := hubWireFrame{Type: "roster", Version: ProtocolVersion, Hosts: snapshot.Hosts, Peers: snapshot.Peers}
	for _, client := range clients {
		if err := client.wire.send(message); err != nil {
			_ = client.wire.connection.Close()
		}
	}
}

func (runtime *hubRuntime) publishStatus(ctx context.Context) error {
	runtime.status.Lock()
	defer runtime.status.Unlock()
	process := procinfo.Read(os.Getpid())
	snapshot := runtime.registry.Snapshot()
	routes := runtime.routes.Counts()
	laneRoutes := runtime.lanes.Counts()
	service := map[string]any{"manager": runtime.options.ServiceManager, "unit": runtime.options.ServiceUnit}
	return saveHubRuntimeStatus(ctx, runtime.paths, HubRuntimeStatus{
		SchemaVersion: 1, RuntimeVersion: runtime.options.RuntimeVersion, RuntimeIdentity: runtime.options.RuntimeIdentity,
		PID: os.Getpid(), ProcStart: process.Start, Listener: runtime.options.Listen, Service: service,
		ProtocolVersion: ProtocolVersion, ConnectedHosts: len(snapshot.Hosts),
		Routing: map[string]any{
			"healthy": true, "revision": snapshot.Revision,
			"pending": routes.Pending, "remote_lanes": laneRoutes,
		},
		Debt: []map[string]any{},
	})
}

func (runtime *hubRuntime) publishStopped(ctx context.Context) error {
	runtime.status.Lock()
	defer runtime.status.Unlock()
	return saveHubRuntimeStatus(ctx, runtime.paths, HubRuntimeStatus{
		SchemaVersion: 1, RuntimeVersion: runtime.options.RuntimeVersion, RuntimeIdentity: runtime.options.RuntimeIdentity,
		Listener:        runtime.options.Listen,
		Service:         map[string]any{"manager": runtime.options.ServiceManager, "unit": runtime.options.ServiceUnit},
		ProtocolVersion: ProtocolVersion, Routing: map[string]any{"healthy": false, "pending": 0},
		Debt: []map[string]any{},
	})
}

func (runtime *hubRuntime) emitStarted(recoveredCrash bool) {
	process := procinfo.Read(os.Getpid())
	fields := map[string]any{
		"operation": "hub.serve", "role": "hub", "state": "ready",
		"runtime_version": runtime.options.RuntimeVersion, "runtime_identity": runtime.options.RuntimeIdentity,
		"pid": os.Getpid(), "proc_start": process.Start, "listener": runtime.options.Listen,
		"protocol_version": ProtocolVersion,
		"service":          map[string]any{"manager": runtime.options.ServiceManager, "unit": runtime.options.ServiceUnit},
	}
	runtime.observability.emit(diagnostics.OutputNormal, "hub.ready", fields)
	runtime.observability.emit(diagnostics.OutputDebug, "hub.recovery", fields)
	runtime.observability.emit(diagnostics.OutputMetric, "hub.metrics", fields)
	runtime.observability.emit(diagnostics.OutputTrace, "hub.traces", fields)
	if recoveredCrash {
		crash := cloneHubMap(fields)
		crash["state"] = "recovered"
		crash["error_code"] = "prior_authority_exited"
		runtime.observability.emit(diagnostics.OutputCrashReport, "hub.crash_recovered", crash)
	}
}

func boundedHubError(err error) string {
	if err == nil {
		return ""
	}
	return diagnosticsBoundedHubError(err.Error())
}

func diagnosticsBoundedHubError(value string) string {
	return diagnostics.BoundedCauseDetail(value)
}
