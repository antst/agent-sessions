package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

	mu       sync.Mutex
	current  map[string]net.Conn
	wg       sync.WaitGroup
	close    sync.Once
	closeErr error
}

func livePresenceEndpoint(stateRoot string) string {
	return filepath.Join(stateRoot, "run", "presence.sock")
}

func startLivePresenceServer(
	ctx context.Context,
	stateRoot string,
	join func(liveSessionReport),
	leave func(liveSessionReport),
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
		listener: listener, join: join, leave: leave,
		current: map[string]net.Conn{},
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
	var report liveSessionReport
	decoder := json.NewDecoder(io.LimitReader(connection, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&report) != nil || !validLiveSessionReport(report) {
		return
	}
	report.Groups = uniqueStrings(report.Groups)
	s.mu.Lock()
	previous := s.current[report.UUID]
	s.current[report.UUID] = connection
	s.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	s.join(report)
	if _, err := io.WriteString(connection, "{}\n"); err == nil {
		_, _ = io.Copy(io.Discard, connection)
	}
	s.mu.Lock()
	current := s.current[report.UUID] == connection
	if current {
		delete(s.current, report.UUID)
	}
	s.mu.Unlock()
	if current {
		s.leave(report)
	}
}

func (s *livePresenceServer) Close() error {
	s.close.Do(func() {
		s.closeErr = s.listener.Close()
		s.mu.Lock()
		connections := make([]net.Conn, 0, len(s.current))
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

func validLiveSessionReport(report liveSessionReport) bool {
	_, supported := productcatalog.ByID(strings.TrimSpace(report.Product))
	return strings.TrimSpace(report.UUID) != "" && supported
}

func maintainLivePresence(ctx context.Context, endpoint string, report liveSessionReport) {
	if !validLiveSessionReport(report) {
		return
	}
	for ctx.Err() == nil {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		if err == nil {
			err = json.NewEncoder(connection).Encode(report)
			if err == nil {
				var ready map[string]any
				err = json.NewDecoder(connection).Decode(&ready)
			}
			if err == nil {
				done := make(chan struct{})
				go func() {
					select {
					case <-ctx.Done():
						_ = connection.Close()
					case <-done:
					}
				}()
				_, _ = io.Copy(io.Discard, connection)
				close(done)
			}
			_ = connection.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
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
			peerIDs[id] = true
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
