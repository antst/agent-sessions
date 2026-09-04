package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/stateroot"
)

type laneNameEntry struct {
	UUID    string
	Name    string
	Product string
}

type livePresenceServer struct {
	listener net.Listener
	join     func(livepresence.Report)
	leave    func(livepresence.Report)
	call     func(context.Context, livepresence.Report, string, string, json.RawMessage) (json.RawMessage, error)

	mu       sync.Mutex
	current  map[string]*livepresence.Connection
	changed  chan struct{}
	wg       sync.WaitGroup
	close    sync.Once
	closeErr error
}

func livePresenceEndpoint(stateRoot string) string {
	return filepath.Join(stateRoot, "run", "presence.sock")
}

func defaultPresenceEndpoint() string {
	endpoint, err := stateroot.PresenceSocket()
	if err != nil {
		return livePresenceEndpoint(defaultStateRoot())
	}
	return endpoint
}

func startLivePresenceServer(
	ctx context.Context,
	stateRoot string,
	join func(livepresence.Report),
	leave func(livepresence.Report),
	call func(context.Context, livepresence.Report, string, string, json.RawMessage) (json.RawMessage, error),
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
		current: map[string]*livepresence.Connection{}, changed: make(chan struct{}),
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
	rpc := livepresence.NewConnection(connection)
	defer rpc.Fail()
	var hello livepresence.Frame
	if rpc.Decode(&hello) != nil || !livepresence.ValidRequest(hello) || hello.Method != "session.hello" {
		return
	}
	report, protocol, err := livepresence.DecodeHello(hello.Params)
	if err != nil {
		_ = rpc.Write(livepresence.Failure(hello.ID, livepresence.InvalidParams, "Invalid params", map[string]any{"method": "session.hello"}))
		return
	}
	if protocol != livepresence.ProtocolVersion {
		_ = rpc.Write(livepresence.Failure(hello.ID, livepresence.UnsupportedVersion, "Unsupported protocol version", map[string]any{
			"supported": livepresence.ProtocolVersion, "received": protocol,
		}))
		return
	}
	rpc.SetReport(report)
	if err := rpc.Write(livepresence.Success(hello.ID, json.RawMessage(`{}`))); err != nil {
		return
	}
	s.mu.Lock()
	// Callbacks take coordinator c.mu and never reenter presence; keep map transitions linear through them.
	previous := s.current[report.UUID]
	s.current[report.UUID] = rpc
	s.signalChangedLocked()
	s.join(report)
	s.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	for {
		var frame livepresence.Frame
		if rpc.Decode(&frame) != nil {
			break
		}
		if !livepresence.ValidFrame(frame) {
			break
		}
		if frame.Method == "" {
			if !rpc.Resolve(frame) {
				break
			}
			continue
		}
		if frame.Method == "session.hello" {
			break
		}
		go s.handleRequest(requestCtx, rpc, frame)
	}
	s.mu.Lock()
	current := s.current[report.UUID] == rpc
	if current {
		delete(s.current, report.UUID)
		s.signalChangedLocked()
		s.leave(rpc.Report())
	}
	s.mu.Unlock()
}

func (s *livePresenceServer) handleRequest(ctx context.Context, connection *livepresence.Connection, frame livepresence.Frame) {
	if frame.Method == "session.update" {
		name, info, err := livepresence.DecodeUpdate(frame.Params)
		if err != nil {
			_ = connection.Write(livepresence.Failure(frame.ID, livepresence.InvalidParams, "Invalid params", map[string]any{"method": frame.Method}))
			return
		}
		s.mu.Lock()
		current := s.current[connection.Report().UUID] == connection
		if current {
			updated := connection.UpdateReport(name, info)
			s.join(updated)
		}
		s.mu.Unlock()
		if !current {
			_ = connection.Write(livepresence.Failure(frame.ID, livepresence.NotPermitted, "Operation not permitted", map[string]any{"method": frame.Method}))
			return
		}
		_ = connection.Write(livepresence.Success(frame.ID, json.RawMessage(`{}`)))
		return
	}
	if s.call == nil {
		_ = connection.Write(livepresence.Failure(frame.ID, livepresence.NotPermitted, "Operation not permitted", map[string]any{"method": frame.Method}))
		return
	}
	result, err := s.call(ctx, connection.Report(), livepresence.IDText(frame.ID), frame.Method, frame.Params)
	if errors.Is(err, daemonpkg.InactiveControlError()) {
		err = &federationpkg.UnknownTargetError{Target: connection.Report().UUID, Detail: "live session attachment is unavailable"}
	}
	response := livepresence.Success(frame.ID, result)
	if err != nil {
		response = livepresence.FailureFromError(frame.ID, frame.Method, err)
	}
	_ = connection.Write(response)
}

func (s *livePresenceServer) Call(ctx context.Context, uuid, id, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	connection := s.current[uuid]
	s.mu.Unlock()
	if connection == nil {
		return nil, &federationpkg.UnknownTargetError{Target: uuid, Detail: "live session is unavailable"}
	}
	if !livepresence.AllowsDaemonMethod(connection.Report(), method) {
		return nil, livepresence.NewError(livepresence.NotPermitted, "Operation not permitted", map[string]any{"method": method})
	}
	return connection.Call(ctx, "daemon."+id, method, params)
}

func (s *livePresenceServer) Wait(ctx context.Context, uuid, product string, lane bool) error {
	for {
		s.mu.Lock()
		connection := s.current[uuid]
		changed := s.changed
		var report livepresence.Report
		if connection != nil {
			report = connection.Report()
		}
		s.mu.Unlock()
		if connection != nil && report.Product == product && (!lane || report.Capabilities.Lane) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (s *livePresenceServer) signalChangedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *livePresenceServer) Close() error {
	s.close.Do(func() {
		s.closeErr = s.listener.Close()
		s.mu.Lock()
		connections := make([]*livepresence.Connection, 0, len(s.current))
		for _, connection := range s.current {
			connections = append(connections, connection)
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

func (c *hostCoordinator) joinLiveSession(runtime *daemonpkg.Runtime, report livepresence.Report) {
	c.mu.Lock()
	if _, existed := c.liveReports[report.UUID]; !existed {
		delete(c.laneNames, report.UUID)
	}
	c.liveReports[report.UUID] = report
	c.mu.Unlock()
	c.syncLiveSessions(runtime)
}

func (c *hostCoordinator) leaveLiveSession(runtime *daemonpkg.Runtime, report livepresence.Report) {
	c.mu.Lock()
	current, ok := c.liveReports[report.UUID]
	departed := ok && current.Product == report.Product
	if departed {
		delete(c.liveReports, report.UUID)
		delete(c.laneNames, report.UUID)
	}
	c.mu.Unlock()
	c.syncLiveSessions(runtime)
	if departed {
		c.archiveIdleLanesForParent(runtime, report.UUID)
	}
}

func (c *hostCoordinator) syncLiveSessions(runtime *daemonpkg.Runtime) {
	hostID := runtime.HostID()
	c.mu.Lock()
	reports := make(map[string]livepresence.Report, len(c.liveReports))
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
		if actorKey == "" && parentID == "" {
			peerIDs[id] = true
			continue
		}
		actor := c.lanes[actorKey]
		if actor == nil {
			actorKey = id
			actor = &laneActor{
				id: id, nativeID: id, groups: append([]string(nil), report.Groups...), done: make(chan struct{}),
			}
			c.lanes[actorKey] = actor
		} else {
			report.Groups = append([]string(nil), actor.groups...)
			reports[id] = report
		}
		actor.product = report.Product
		actor.name = report.Name
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
			UUID: id, Name: actor.name, Product: actor.product,
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
		runtime.Attachments().ReportLive(id, report.Name, report.Product, report.Groups, report.Info, laneParents[id] != "")
	}
}

func (c *hostCoordinator) reportedLaneContextLocked(nativeID string) (string, string) {
	if actorKey := c.reportedLanes[nativeID]; actorKey != "" {
		if actor := c.lanes[actorKey]; actor != nil {
			return actorKey, actor.parentID
		}
	}
	for actorKey, actor := range c.lanes {
		if actor != nil && actor.id == nativeID && actor.parentID != "" {
			return actorKey, actor.parentID
		}
	}
	return "", ""
}

func (c *hostCoordinator) unboundReportedLaneContextLocked(parentID string, report livepresence.Report) string {
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

func reportParentFromGroups(hostID string, report livepresence.Report, reports map[string]livepresence.Report) string {
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
