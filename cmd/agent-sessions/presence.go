package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/sessiontools"
	"github.com/antst/agent-sessions/internal/stateroot"
)

const (
	liveProtocolVersion          = 1
	liveSessionReconnectInterval = 2 * time.Second
)

const (
	liveRPCInvalidParams      = -32602
	liveRPCUnknown            = -32001
	liveRPCBusy               = -32002
	liveRPCNotPermitted       = -32003
	liveRPCUnsupportedVersion = -32004
	liveRPCProductUnavailable = -32005
	liveRPCProductFailure     = -32006
)

var (
	errLiveInvalidParams      = errors.New("invalid params")
	errLiveUnknown            = errors.New("unknown session or target")
	errLiveBusy               = errors.New("session busy")
	errLiveNotPermitted       = errors.New("operation not permitted")
	errLiveNoRunningTurn      = errors.New("no running turn")
	errLiveProductUnavailable = errors.New("product not launchable")
)

type liveClassifiedError struct {
	class error
	cause error
}

func (e liveClassifiedError) Error() string { return e.cause.Error() }
func (e liveClassifiedError) Unwrap() error { return e.class }

func classifyLiveError(class, cause error) error {
	if cause == nil {
		cause = class
	}
	return liveClassifiedError{class: class, cause: cause}
}

// liveSessionReport is the complete reconnect vocabulary. A connection that
// carries this report is live; closing the connection removes the report.
type liveSessionReport struct {
	UUID         string                  `json:"uuid"`
	Name         string                  `json:"name"`
	Groups       []string                `json:"groups"`
	Product      string                  `json:"product"`
	Info         map[string]string       `json:"info"`
	Capabilities liveSessionCapabilities `json:"capabilities,omitempty"`
}

type liveSessionCapabilities struct {
	Lane bool `json:"lane,omitempty"`
}

type laneNameEntry struct {
	UUID    string
	Name    string
	Product string
}

type livePresenceServer struct {
	listener net.Listener
	join     func(liveSessionReport)
	leave    func(liveSessionReport)
	call     func(context.Context, liveSessionReport, string, string, json.RawMessage) (json.RawMessage, error)

	mu       sync.Mutex
	current  map[string]*liveRPCConnection
	changed  chan struct{}
	wg       sync.WaitGroup
	close    sync.Once
	closeErr error
}

// liveRPCFrame is the complete newline-delimited JSON-RPC 2.0 vocabulary.
type liveRPCFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *liveRPCError   `json:"error,omitempty"`
}

type liveRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *liveRPCError) Error() string {
	if e == nil {
		return "live RPC error"
	}
	return e.Message
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
	wireID, _ := json.Marshal(id)
	key := string(wireID)
	response := make(chan liveRPCFrame, 1)
	c.mu.Lock()
	if _, duplicate := c.pending[key]; duplicate {
		c.mu.Unlock()
		return nil, errors.New("live RPC request id is already outstanding")
	}
	c.pending[key] = response
	c.mu.Unlock()
	if err := c.write(liveRPCFrame{JSONRPC: "2.0", ID: wireID, Method: method, Params: body}); err != nil {
		c.drop(key)
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.drop(key)
		return nil, ctx.Err()
	case frame, ok := <-response:
		if !ok {
			return nil, errors.New("live session disconnected")
		}
		if frame.Error != nil {
			return nil, frame.Error
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
	key := string(frame.ID)
	c.mu.Lock()
	response := c.pending[key]
	delete(c.pending, key)
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
	return cloneLiveSessionReport(c.report)
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
	join func(liveSessionReport),
	leave func(liveSessionReport),
	call func(context.Context, liveSessionReport, string, string, json.RawMessage) (json.RawMessage, error),
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
		current: map[string]*liveRPCConnection{}, changed: make(chan struct{}),
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
	decoder := rpc.decoder
	decoder.DisallowUnknownFields()
	var hello liveRPCFrame
	if decoder.Decode(&hello) != nil || !validLiveRPCRequest(hello) || hello.Method != "session.hello" {
		return
	}
	report, protocol, err := decodeLiveHello(hello.Params)
	if err != nil {
		_ = rpc.write(liveRPCFailure(hello.ID, liveRPCInvalidParams, "Invalid params", map[string]any{"method": "session.hello"}))
		return
	}
	if protocol != liveProtocolVersion {
		_ = rpc.write(liveRPCFailure(hello.ID, liveRPCUnsupportedVersion, "Unsupported protocol version", map[string]any{
			"supported": liveProtocolVersion, "received": protocol,
		}))
		return
	}
	rpc.report = cloneLiveSessionReport(report)
	if err := rpc.write(liveRPCSuccess(hello.ID, json.RawMessage(`{}`))); err != nil {
		return
	}
	s.mu.Lock()
	previous := s.current[report.UUID]
	s.current[report.UUID] = rpc
	s.signalChangedLocked()
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
		if !validLiveRPCFrame(frame) {
			break
		}
		if frame.Method == "" {
			if !rpc.resolve(frame) {
				break
			}
			continue
		}
		go s.handleRequest(requestCtx, rpc, frame)
	}
	s.mu.Lock()
	current := s.current[report.UUID] == rpc
	if current {
		delete(s.current, report.UUID)
		s.signalChangedLocked()
	}
	s.mu.Unlock()
	if current {
		s.leave(rpc.liveReport())
	}
}

func (s *livePresenceServer) handleRequest(ctx context.Context, connection *liveRPCConnection, frame liveRPCFrame) {
	if frame.Method == "session.update" {
		name, info, err := decodeLiveUpdate(frame.Params)
		if err != nil {
			_ = connection.write(liveRPCFailure(frame.ID, liveRPCInvalidParams, "Invalid params", map[string]any{"method": frame.Method}))
			return
		}
		s.mu.Lock()
		current := s.current[connection.report.UUID] == connection
		s.mu.Unlock()
		if !current {
			return
		}
		connection.mu.Lock()
		updated := cloneLiveSessionReport(connection.report)
		updated.Name = name
		updated.Info = cloneLiveInfo(info)
		connection.report = updated
		connection.mu.Unlock()
		s.join(updated)
		_ = connection.write(liveRPCSuccess(frame.ID, json.RawMessage(`{}`)))
		return
	}
	if s.call == nil {
		_ = connection.write(liveRPCFailure(frame.ID, liveRPCNotPermitted, "Operation not permitted", map[string]any{"method": frame.Method}))
		return
	}
	result, err := s.call(ctx, connection.liveReport(), liveRPCIDText(frame.ID), frame.Method, frame.Params)
	response := liveRPCSuccess(frame.ID, result)
	if err != nil {
		response = liveRPCFailureFromError(frame.ID, frame.Method, err)
	}
	_ = connection.write(response)
}

func (s *livePresenceServer) Call(ctx context.Context, uuid, id, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	connection := s.current[uuid]
	s.mu.Unlock()
	if connection == nil {
		return nil, classifyLiveError(errLiveUnknown, errors.New("live session is unavailable"))
	}
	return connection.call(ctx, "daemon."+id, method, params)
}

func (s *livePresenceServer) Wait(ctx context.Context, uuid, product string, lane bool) error {
	for {
		s.mu.Lock()
		connection := s.current[uuid]
		changed := s.changed
		var report liveSessionReport
		if connection != nil {
			report = connection.liveReport()
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
	return strings.TrimSpace(report.UUID) != "" && strings.TrimSpace(report.Product) != "" && report.Groups != nil && report.Info != nil
}

func cloneLiveSessionReport(report liveSessionReport) liveSessionReport {
	groups := make([]string, len(report.Groups))
	copy(groups, report.Groups)
	report.Groups = groups
	report.Info = cloneLiveInfo(report.Info)
	return report
}

func cloneLiveInfo(info map[string]string) map[string]string {
	cloned := make(map[string]string, len(info))
	for key, value := range info {
		cloned[key] = value
	}
	return cloned
}

func liveCwdInfo(cwd string) map[string]string {
	info := map[string]string{}
	if strings.TrimSpace(cwd) != "" {
		info["cwd"] = cwd
	}
	return info
}

func validLiveRPCRequest(frame liveRPCFrame) bool {
	return frame.JSONRPC == "2.0" && validLiveRPCID(frame.ID) && strings.TrimSpace(frame.Method) != "" &&
		len(frame.Params) != 0 && json.Valid(frame.Params) && frame.Result == nil && frame.Error == nil
}

func validLiveRPCFrame(frame liveRPCFrame) bool {
	if validLiveRPCRequest(frame) {
		return true
	}
	if frame.JSONRPC != "2.0" || !validLiveRPCID(frame.ID) || frame.Method != "" || frame.Params != nil {
		return false
	}
	return frame.Result != nil && frame.Error == nil || frame.Result == nil && validLiveRPCError(frame.Error)
}

func validLiveRPCID(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func liveRPCIDText(raw json.RawMessage) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	return fmt.Sprint(value)
}

func validLiveRPCError(rpcErr *liveRPCError) bool {
	if rpcErr == nil || strings.TrimSpace(rpcErr.Message) == "" {
		return false
	}
	switch rpcErr.Code {
	case liveRPCInvalidParams, liveRPCUnknown, liveRPCBusy, liveRPCNotPermitted,
		liveRPCUnsupportedVersion, liveRPCProductUnavailable, liveRPCProductFailure:
		return rpcErr.Data == nil || json.Valid(rpcErr.Data)
	default:
		return false
	}
}

func decodeLiveHello(raw json.RawMessage) (liveSessionReport, int, error) {
	var params struct {
		Protocol     *int                     `json:"protocol"`
		UUID         *string                  `json:"uuid"`
		Name         *string                  `json:"name"`
		Groups       *[]string                `json:"groups"`
		Product      *string                  `json:"product"`
		Info         *map[string]string       `json:"info"`
		Capabilities *liveSessionCapabilities `json:"capabilities,omitempty"`
	}
	if err := decodeStrictJSON(raw, &params); err != nil || params.Protocol == nil || params.UUID == nil || params.Name == nil ||
		params.Groups == nil || params.Product == nil || params.Info == nil || params.Capabilities != nil && !params.Capabilities.Lane {
		return liveSessionReport{}, 0, errors.New("session hello is invalid")
	}
	groups := make([]string, len(*params.Groups))
	copy(groups, *params.Groups)
	report := liveSessionReport{
		UUID: *params.UUID, Name: *params.Name, Groups: groups,
		Product: *params.Product, Info: cloneLiveInfo(*params.Info),
	}
	if params.Capabilities != nil {
		report.Capabilities = *params.Capabilities
	}
	if !validLiveSessionReport(report) {
		return liveSessionReport{}, *params.Protocol, errors.New("session hello is invalid")
	}
	return report, *params.Protocol, nil
}

func decodeLiveUpdate(raw json.RawMessage) (string, map[string]string, error) {
	var params struct {
		Name *string            `json:"name"`
		Info *map[string]string `json:"info"`
	}
	if err := decodeStrictJSON(raw, &params); err != nil || params.Name == nil || params.Info == nil {
		return "", nil, errors.New("session update is invalid")
	}
	return *params.Name, cloneLiveInfo(*params.Info), nil
}

func decodeStrictJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON value has trailing content")
	}
	return nil
}

func liveRPCSuccess(id, result json.RawMessage) liveRPCFrame {
	if result == nil {
		result = json.RawMessage(`null`)
	}
	return liveRPCFrame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Result: result}
}

func liveRPCFailure(id json.RawMessage, code int, message string, data any) liveRPCFrame {
	var encoded json.RawMessage
	if data != nil {
		encoded, _ = json.Marshal(data)
	}
	return liveRPCFrame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Error: &liveRPCError{
		Code: code, Message: message, Data: encoded,
	}}
}

func newLiveRPCError(code int, message string, data any) error {
	frame := liveRPCFailure(json.RawMessage(`0`), code, message, data)
	return frame.Error
}

func liveRPCFailureFromError(id json.RawMessage, method string, err error) liveRPCFrame {
	var remote *liveRPCError
	if errors.As(err, &remote) && validLiveRPCError(remote) {
		return liveRPCFrame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Error: remote}
	}
	if errors.Is(err, daemonpkg.InactiveControlError()) || errors.Is(err, federationpkg.ErrUnknownTarget) || errors.Is(err, errLiveUnknown) {
		return liveRPCFailure(id, liveRPCUnknown, "Unknown session or target", map[string]any{"detail": err.Error()})
	}
	if errors.Is(err, errLiveInvalidParams) {
		return liveRPCFailure(id, liveRPCInvalidParams, "Invalid params", map[string]any{"detail": err.Error()})
	}
	if errors.Is(err, errLiveNoRunningTurn) {
		return liveRPCFailure(id, liveRPCNotPermitted, "Operation not permitted", map[string]any{"reason": "no running turn"})
	}
	if errors.Is(err, productruntime.ErrUnsupportedSteer) {
		return liveRPCFailure(id, liveRPCNotPermitted, "Operation not permitted", map[string]any{"reason": "steer unsupported"})
	}
	if errors.Is(err, errLiveNotPermitted) || errors.Is(err, productruntime.ErrUnauthorized) ||
		errors.Is(err, productruntime.ErrUnsupportedPolicy) || errors.Is(err, productruntime.ErrUnsupportedRename) {
		return liveRPCFailure(id, liveRPCNotPermitted, "Operation not permitted", map[string]any{"detail": err.Error()})
	}
	if errors.Is(err, errLiveBusy) || errors.Is(err, productruntime.ErrAmbiguousSession) {
		return liveRPCFailure(id, liveRPCBusy, "Session busy", map[string]any{"detail": err.Error()})
	}
	if errors.Is(err, errLiveProductUnavailable) || errors.Is(err, productruntime.ErrUnavailable) || errors.Is(err, productruntime.ErrIncompatible) {
		return liveRPCFailure(id, liveRPCProductUnavailable, "Product not launchable", map[string]any{"detail": err.Error()})
	}
	return liveRPCFailure(id, liveRPCProductFailure, err.Error(), map[string]any{
		"detail": err.Error(), "agent_sessions_bug_report": sessiontools.BugReportGuidance,
	})
}

type liveSessionClient struct {
	ctx           context.Context
	endpoint      string
	report        liveSessionReport
	resolveReport func(context.Context) (liveSessionReport, bool)

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
	report = normalizeLiveSessionReport(report)
	client := &liveSessionClient{ctx: ctx, endpoint: endpoint, report: report, call: call}
	if validLiveSessionReport(report) {
		go client.run()
	}
	return client
}

func startResolvingLiveSessionClient(
	ctx context.Context,
	endpoint string,
	resolveReport func(context.Context) (liveSessionReport, bool),
	call func(context.Context, string, json.RawMessage) (json.RawMessage, error),
) *liveSessionClient {
	client := &liveSessionClient{ctx: ctx, endpoint: endpoint, resolveReport: resolveReport, call: call}
	go client.run()
	return client
}

func (c *liveSessionClient) Call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	connection := c.current
	resolvesProductIdentity := c.resolveReport != nil
	c.mu.Unlock()
	if connection == nil {
		if resolvesProductIdentity {
			return nil, errors.New("live session identity is not confirmed by the product")
		}
		return nil, errors.New("Agent Sessions daemon is unavailable")
	}
	return connection.call(ctx, "session."+id, method, params)
}

func (c *liveSessionClient) UpdateReport(ctx context.Context, report liveSessionReport) error {
	report = normalizeLiveSessionReport(report)
	if !validLiveSessionReport(report) {
		return errors.New("live session update is invalid")
	}
	c.mu.Lock()
	c.report = cloneLiveSessionReport(report)
	connection := c.current
	c.mu.Unlock()
	if connection == nil {
		return nil
	}
	_, err := connection.call(ctx, "session.update", "session.update", map[string]any{"name": report.Name, "info": report.Info})
	return err
}

func normalizeLiveSessionReport(report liveSessionReport) liveSessionReport {
	if report.Groups == nil {
		report.Groups = []string{}
	}
	if report.Info == nil {
		report.Info = map[string]string{}
	}
	return cloneLiveSessionReport(report)
}

func (c *liveSessionClient) run() {
	for c.ctx.Err() == nil {
		connection, err := (&net.Dialer{}).DialContext(c.ctx, "unix", c.endpoint)
		if err == nil {
			c.mu.Lock()
			report := cloneLiveSessionReport(c.report)
			resolveReport := c.resolveReport
			c.mu.Unlock()
			confirmed := true
			if resolveReport != nil {
				report, confirmed = resolveReport(c.ctx)
			}
			report = normalizeLiveSessionReport(report)
			if !confirmed || !validLiveSessionReport(report) {
				_ = connection.Close()
			} else {
				rpc := newLiveRPCConnection(connection)
				rpc.decoder.DisallowUnknownFields()
				closed := make(chan struct{})
				go func() {
					select {
					case <-c.ctx.Done():
						_ = connection.Close()
					case <-closed:
					}
				}()
				c.mu.Lock()
				c.report = cloneLiveSessionReport(report)
				err = liveSessionHello(rpc, report)
				if err == nil {
					rpc.report = cloneLiveSessionReport(report)
					c.current = rpc
				}
				c.mu.Unlock()
				if err == nil {
					readCtx, stopReads := context.WithCancel(c.ctx)
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
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(liveSessionReconnectInterval):
		}
	}
}

func (c *liveSessionClient) read(ctx context.Context, connection *liveRPCConnection) {
	for {
		var frame liveRPCFrame
		if connection.decoder.Decode(&frame) != nil {
			return
		}
		if !validLiveRPCFrame(frame) {
			return
		}
		if frame.Method == "" {
			if !connection.resolve(frame) {
				return
			}
			continue
		}
		go func(frame liveRPCFrame) {
			response := liveRPCSuccess(frame.ID, nil)
			if c.call == nil {
				response = liveRPCFailure(frame.ID, liveRPCNotPermitted, "Operation not permitted", map[string]any{"method": frame.Method})
			} else {
				result, err := c.call(ctx, frame.Method, frame.Params)
				if err != nil {
					response = liveRPCFailureFromError(frame.ID, frame.Method, err)
				} else {
					response = liveRPCSuccess(frame.ID, result)
				}
			}
			_ = connection.write(response)
		}(frame)
	}
}

func liveSessionHello(connection *liveRPCConnection, report liveSessionReport) error {
	id := json.RawMessage(`"session.hello"`)
	hello := map[string]any{
		"protocol": liveProtocolVersion, "uuid": report.UUID, "name": report.Name,
		"groups": report.Groups, "product": report.Product, "info": report.Info,
	}
	if report.Capabilities.Lane {
		hello["capabilities"] = report.Capabilities
	}
	params, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	if err := connection.write(liveRPCFrame{JSONRPC: "2.0", ID: id, Method: "session.hello", Params: params}); err != nil {
		return err
	}
	var response liveRPCFrame
	if err := connection.decoder.Decode(&response); err != nil || !validLiveRPCFrame(response) || response.Method != "" || string(response.ID) != string(id) {
		return errors.New("live session hello returned an invalid response")
	}
	if response.Error != nil {
		return response.Error
	}
	return nil
}

func maintainLivePresence(ctx context.Context, endpoint string, report liveSessionReport) {
	report = normalizeLiveSessionReport(report)
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
	}
	c.liveReports[report.UUID] = report
	c.mu.Unlock()
	c.syncLiveSessions(runtime)
}

func (c *hostCoordinator) leaveLiveSession(runtime *daemonpkg.Runtime, report liveSessionReport) {
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
