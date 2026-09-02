package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

// liveSessionReport is the complete reconnect vocabulary. A connection that
// carries this report is live; closing the connection removes the report.
type liveSessionReport struct {
	UUID    string   `json:"uuid"`
	Name    string   `json:"name"`
	Groups  []string `json:"groups"`
	Product string   `json:"product"`
}

type laneNameEntry struct {
	UUID            string
	Name            string
	Product         string
	Parent          string
	Groups          []string
	SecondaryGroups []string
	Cwd             string
}

type livePresenceServer struct {
	listener net.Listener
	join     func(liveSessionReport)
	leave    func(liveSessionReport)
	call     func(context.Context, liveSessionReport, string, json.RawMessage) (json.RawMessage, error)

	mu       sync.Mutex
	current  map[string]*liveRPCConnection
	wg       sync.WaitGroup
	close    sync.Once
	closeErr error
}

// liveRPCFrame is the whole post-report vocabulary. A method names a request;
// a method-less frame is the response bearing either result or error.
type liveRPCFrame struct {
	ID     string          `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type liveRPCConnection struct {
	connection net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan liveRPCFrame
	report  liveSessionReport
}

func newLiveRPCConnection(connection net.Conn) *liveRPCConnection {
	return &liveRPCConnection{
		connection: connection, encoder: json.NewEncoder(connection), decoder: json.NewDecoder(bufio.NewReader(connection)),
		pending: map[string]chan liveRPCFrame{},
	}
}

func (c *liveRPCConnection) write(frame liveRPCFrame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.encoder.Encode(frame)
}

func (c *liveRPCConnection) call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(method) == "" {
		return nil, errors.New("live RPC request identity is incomplete")
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	response := make(chan liveRPCFrame, 1)
	c.mu.Lock()
	if _, duplicate := c.pending[id]; duplicate {
		c.mu.Unlock()
		return nil, errors.New("live RPC request id is already outstanding")
	}
	c.pending[id] = response
	c.mu.Unlock()
	if err := c.write(liveRPCFrame{ID: id, Method: method, Params: body}); err != nil {
		c.drop(id)
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.drop(id)
		return nil, ctx.Err()
	case frame, ok := <-response:
		if !ok {
			return nil, errors.New("live session disconnected")
		}
		if frame.Error != "" {
			return nil, errors.New(frame.Error)
		}
		return append(json.RawMessage(nil), frame.Result...), nil
	}
}

func (c *liveRPCConnection) drop(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *liveRPCConnection) resolve(frame liveRPCFrame) bool {
	c.mu.Lock()
	response := c.pending[frame.ID]
	delete(c.pending, frame.ID)
	c.mu.Unlock()
	if response == nil {
		return false
	}
	response <- frame
	close(response)
	return true
}

func (c *liveRPCConnection) fail() {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]chan liveRPCFrame{}
	c.mu.Unlock()
	for _, response := range pending {
		close(response)
	}
}

func (c *liveRPCConnection) liveReport() liveSessionReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.report
}

func livePresenceEndpoint(stateRoot string) string {
	return filepath.Join(stateRoot, "run", "presence.sock")
}

func startLivePresenceServer(
	ctx context.Context,
	stateRoot string,
	join func(liveSessionReport),
	leave func(liveSessionReport),
	call func(context.Context, liveSessionReport, string, json.RawMessage) (json.RawMessage, error),
) (*livePresenceServer, error) {
	endpoint := livePresenceEndpoint(stateRoot)
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(endpoint)
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &livePresenceServer{
		listener: listener, join: join, leave: leave, call: call,
		current: map[string]*liveRPCConnection{},
	}
	server.wg.Add(1)
	go server.accept()
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	return server, nil
}

func (s *livePresenceServer) accept() {
	defer s.wg.Done()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serve(connection)
	}
}

func (s *livePresenceServer) serve(connection net.Conn) {
	defer s.wg.Done()
	defer func() { _ = connection.Close() }()
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	rpc := newLiveRPCConnection(connection)
	defer rpc.fail()
	var report liveSessionReport
	decoder := rpc.decoder
	decoder.DisallowUnknownFields()
	if decoder.Decode(&report) != nil || !validLiveSessionReport(report) {
		return
	}
	report.Groups = uniqueStrings(report.Groups)
	rpc.report = report
	s.mu.Lock()
	previous := s.current[report.UUID]
	s.current[report.UUID] = rpc
	s.mu.Unlock()
	if previous != nil {
		_ = previous.connection.Close()
	}
	s.join(report)
	for {
		var frame liveRPCFrame
		if decoder.Decode(&frame) != nil {
			break
		}
		if strings.TrimSpace(frame.ID) == "" {
			break
		}
		if frame.Method == "" {
			rpc.resolve(frame)
			continue
		}
		go s.handleRequest(requestCtx, rpc, report, frame)
	}
	s.mu.Lock()
	current := s.current[report.UUID] == rpc
	if current {
		delete(s.current, report.UUID)
	}
	s.mu.Unlock()
	if current {
		s.leave(rpc.liveReport())
	}
}

func (s *livePresenceServer) handleRequest(ctx context.Context, connection *liveRPCConnection, report liveSessionReport, frame liveRPCFrame) {
	if frame.Method == "session.update" {
		var updated liveSessionReport
		decoder := json.NewDecoder(strings.NewReader(string(frame.Params)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&updated) != nil || !validLiveSessionReport(updated) ||
			updated.UUID != report.UUID || updated.Product != report.Product {
			_ = connection.write(liveRPCFrame{ID: frame.ID, Error: "live session update is invalid"})
			return
		}
		updated.Groups = uniqueStrings(updated.Groups)
		connection.mu.Lock()
		connection.report = updated
		connection.mu.Unlock()
		s.join(updated)
		_ = connection.write(liveRPCFrame{ID: frame.ID, Result: json.RawMessage(`{}`)})
		return
	}
	if s.call == nil {
		_ = connection.write(liveRPCFrame{ID: frame.ID, Error: "live session operation is unsupported"})
		return
	}
	result, err := s.call(ctx, connection.liveReport(), frame.Method, frame.Params)
	response := liveRPCFrame{ID: frame.ID, Result: result}
	if err != nil {
		response.Result = nil
		response.Error = err.Error()
	}
	_ = connection.write(response)
}

func (s *livePresenceServer) Call(ctx context.Context, uuid, id, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	connection := s.current[uuid]
	s.mu.Unlock()
	if connection == nil {
		return nil, errors.New("live session is unavailable")
	}
	return connection.call(ctx, "daemon."+id, method, params)
}

func (s *livePresenceServer) Close() error {
	s.close.Do(func() {
		s.closeErr = s.listener.Close()
		s.mu.Lock()
		connections := make([]net.Conn, 0, len(s.current))
		for _, connection := range s.current {
			connections = append(connections, connection.connection)
		}
		s.mu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
		s.wg.Wait()
		if errors.Is(s.closeErr, net.ErrClosed) {
			s.closeErr = nil
		}
	})
	return s.closeErr
}

func validLiveSessionReport(report liveSessionReport) bool {
	_, supported := productcatalog.ByID(strings.TrimSpace(report.Product))
	return strings.TrimSpace(report.UUID) != "" && supported
}

type liveSessionClient struct {
	ctx      context.Context
	endpoint string
	report   liveSessionReport

	mu      sync.Mutex
	current *liveRPCConnection
	call    func(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

func startLiveSessionClient(
	ctx context.Context,
	endpoint string,
	report liveSessionReport,
	call func(context.Context, string, json.RawMessage) (json.RawMessage, error),
) *liveSessionClient {
	client := &liveSessionClient{ctx: ctx, endpoint: endpoint, report: report, call: call}
	go client.run()
	return client
}

func (c *liveSessionClient) Call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	connection := c.current
	c.mu.Unlock()
	if connection == nil {
		return nil, errors.New("Agent Sessions daemon is unavailable")
	}
	return connection.call(ctx, "session."+id, method, params)
}

func (c *liveSessionClient) UpdateReport(ctx context.Context, report liveSessionReport) error {
	if !validLiveSessionReport(report) {
		return errors.New("live session update is invalid")
	}
	c.mu.Lock()
	c.report = report
	connection := c.current
	c.mu.Unlock()
	if connection == nil {
		return nil
	}
	_, err := connection.call(ctx, "session.update", "session.update", report)
	return err
}

func (c *liveSessionClient) run() {
	c.mu.Lock()
	report := c.report
	c.mu.Unlock()
	if !validLiveSessionReport(report) {
		return
	}
	for c.ctx.Err() == nil {
		connection, err := (&net.Dialer{}).DialContext(c.ctx, "unix", c.endpoint)
		if err == nil {
			rpc := newLiveRPCConnection(connection)
			closed := make(chan struct{})
			go func() {
				select {
				case <-c.ctx.Done():
					_ = connection.Close()
				case <-closed:
				}
			}()
			c.mu.Lock()
			report = c.report
			c.mu.Unlock()
			err = rpc.encoder.Encode(report)
			if err == nil {
				readCtx, stopReads := context.WithCancel(c.ctx)
				c.mu.Lock()
				c.current = rpc
				c.mu.Unlock()
				c.read(readCtx, rpc)
				stopReads()
				c.mu.Lock()
				if c.current == rpc {
					c.current = nil
				}
				c.mu.Unlock()
			}
			rpc.fail()
			_ = connection.Close()
			close(closed)
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *liveSessionClient) read(ctx context.Context, connection *liveRPCConnection) {
	for {
		var frame liveRPCFrame
		if connection.decoder.Decode(&frame) != nil {
			return
		}
		if frame.Method == "" {
			connection.resolve(frame)
			continue
		}
		go func(frame liveRPCFrame) {
			response := liveRPCFrame{ID: frame.ID}
			if c.call == nil {
				response.Error = "live session operation is unsupported"
			} else {
				response.Result, response.Error = liveCall(ctx, c.call, frame.Method, frame.Params)
			}
			_ = connection.write(response)
		}(frame)
	}
}

func liveCall(
	ctx context.Context,
	call func(context.Context, string, json.RawMessage) (json.RawMessage, error),
	method string,
	params json.RawMessage,
) (json.RawMessage, string) {
	result, err := call(ctx, method, params)
	if err != nil {
		return nil, err.Error()
	}
	return result, ""
}

func maintainLivePresence(ctx context.Context, endpoint string, report liveSessionReport) {
	if !validLiveSessionReport(report) {
		return
	}
	client := startLiveSessionClient(ctx, endpoint, report, nil)
	<-ctx.Done()
	client.mu.Lock()
	if client.current != nil {
		_ = client.current.connection.Close()
	}
	client.mu.Unlock()
}

func (c *hostCoordinator) joinLiveSession(runtime *daemonpkg.Runtime, report liveSessionReport) {
	c.mu.Lock()
	if _, existed := c.liveReports[report.UUID]; !existed {
		delete(c.laneNames, report.UUID)
		delete(c.laneNamesLoaded, report.UUID)
	}
	c.liveReports[report.UUID] = report
	c.mu.Unlock()
	c.syncLiveSessions(runtime)
}

func (c *hostCoordinator) leaveLiveSession(runtime *daemonpkg.Runtime, report liveSessionReport) {
	c.mu.Lock()
	current, ok := c.liveReports[report.UUID]
	if ok && current.Product == report.Product {
		delete(c.liveReports, report.UUID)
		delete(c.laneNames, report.UUID)
		delete(c.laneNamesLoaded, report.UUID)
	}
	c.mu.Unlock()
	c.syncLiveSessions(runtime)
}

func (c *hostCoordinator) syncLiveSessions(runtime *daemonpkg.Runtime) {
	hostID := runtime.HostID()
	c.mu.Lock()
	reports := make(map[string]liveSessionReport, len(c.liveReports))
	for id, report := range c.liveReports {
		reports[id] = report
	}
	oldPeers := c.reportedPeers
	oldLanes := c.reportedLanes
	peerIDs := map[string]bool{}
	laneKeys := map[string]string{}
	laneParents := map[string]string{}

	for id, report := range reports {
		actorKey, parentID := c.reportedLaneContextLocked(id)
		if parentID == "" {
			parentID = reportParentFromGroups(hostID, report, reports)
		}
		if actorKey == "" && parentID != "" {
			actorKey = c.unboundReportedLaneContextLocked(parentID, report)
		}
		if parentID == "" {
			product, _ := productcatalog.ByID(report.Product)
			if product.Has(productcatalog.CapabilityInteractive) {
				peerIDs[id] = true
			}
			continue
		}
		actor := c.lanes[actorKey]
		if actor == nil {
			actorKey = id
			actor = &laneActor{id: id, nativeID: id, done: make(chan struct{})}
			c.lanes[actorKey] = actor
		}
		actor.product = report.Product
		actor.name = report.Name
		actor.groups = append([]string(nil), report.Groups...)
		actor.parentID = parentID
		actor.nativeID = id
		if actor.state == "" {
			actor.state = "idle"
		}
		laneKeys[id] = actorKey
		laneParents[id] = parentID
	}

	for id, actorKey := range oldLanes {
		if _, ok := laneKeys[id]; !ok {
			delete(c.lanes, actorKey)
		}
	}
	for id := range oldPeers {
		if !peerIDs[id] {
			delete(c.laneNames, id)
			delete(c.laneNamesLoaded, id)
		}
	}
	for id, actorKey := range laneKeys {
		actor := c.lanes[actorKey]
		if actor == nil || !peerIDs[actor.parentID] {
			continue
		}
		if c.laneNames[actor.parentID] == nil {
			c.laneNames[actor.parentID] = map[string]laneNameEntry{}
		}
		c.laneNames[actor.parentID][id] = laneNameEntry{
			UUID: id, Name: actor.name, Product: actor.product, Parent: actor.parentID,
			Groups: append([]string(nil), actor.groups...),
		}
	}
	c.reportedPeers = peerIDs
	c.reportedLanes = laneKeys
	c.mu.Unlock()

	for id := range oldPeers {
		if _, present := reports[id]; !present {
			runtime.Attachments().ForgetLive(id)
		}
	}
	for id := range oldLanes {
		if _, present := reports[id]; !present {
			runtime.Attachments().ForgetLive(id)
		}
	}
	for id, report := range reports {
		if !peerIDs[id] && laneKeys[id] == "" {
			runtime.Attachments().ForgetLive(id)
			continue
		}
		runtime.Attachments().ReportLive(id, report.Name, report.Product, report.Groups, laneParents[id] != "")
	}
}

func (c *hostCoordinator) reportedLaneContextLocked(nativeID string) (string, string) {
	if actorKey := c.reportedLanes[nativeID]; actorKey != "" {
		if actor := c.lanes[actorKey]; actor != nil {
			return actorKey, actor.parentID
		}
	}
	for actorKey, actor := range c.lanes {
		if actor != nil && (actor.id == nativeID || actor.nativeID == nativeID) && actor.parentID != "" {
			return actorKey, actor.parentID
		}
	}
	return "", ""
}

func (c *hostCoordinator) unboundReportedLaneContextLocked(parentID string, report liveSessionReport) string {
	for actorKey, actor := range c.lanes {
		if actor == nil || actor.parentID != parentID || actor.product != report.Product || actor.name != report.Name {
			continue
		}
		if actor.nativeID == "" || actor.nativeID == actor.id {
			return actorKey
		}
	}
	return ""
}

func reportParentFromGroups(hostID string, report liveSessionReport, reports map[string]liveSessionReport) string {
	for id := range reports {
		if id == report.UUID {
			continue
		}
		anchor := "session:" + hostID + "/" + id
		for _, group := range report.Groups {
			if group == anchor {
				return id
			}
		}
	}
	return ""
}
