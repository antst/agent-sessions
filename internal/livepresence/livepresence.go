package livepresence

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/sessionidentity"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

const (
	ProtocolVersion   = 1
	ReconnectInterval = 2 * time.Second

	InvalidParams      = -32602
	Unknown            = -32001
	Busy               = -32002
	NotPermitted       = -32003
	UnsupportedVersion = -32004
	ProductUnavailable = -32005
	ProductFailure     = -32006
)

var (
	ErrInvalidParams      = errors.New("invalid params")
	ErrUnknown            = errors.New("unknown session or target")
	ErrBusy               = errors.New("session busy")
	ErrNotPermitted       = errors.New("operation not permitted")
	ErrNoRunningTurn      = errors.New("no running turn")
	ErrProductUnavailable = errors.New("product not launchable")
)

type classifiedError struct {
	class   error
	cause   error
	uuid    string
	product string
}

func (e classifiedError) Error() string { return e.cause.Error() }
func (e classifiedError) Unwrap() error { return e.class }

func ClassifyError(class, cause error) error {
	if cause == nil {
		cause = class
	}
	return classifiedError{class: class, cause: cause}
}

func BusyError(uuid string, cause error) error { return classifiedError{ErrBusy, cause, uuid, ""} }
func ProductError(product string, err error) error {
	if !errors.Is(err, ErrProductUnavailable) && !errors.Is(err, productruntime.ErrUnavailable) && !errors.Is(err, productruntime.ErrIncompatible) {
		return err
	}
	return classifiedError{ErrProductUnavailable, err, "", product}
}

type Report struct {
	UUID         string            `json:"uuid"`
	Name         string            `json:"name"`
	Groups       []string          `json:"groups"`
	Product      string            `json:"product"`
	Info         map[string]string `json:"info"`
	Capabilities Capabilities      `json:"capabilities,omitempty"`
}

type Capabilities struct {
	Lane bool `json:"lane,omitempty"`
}

type Frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "live RPC error"
	}
	return e.Message
}

func (e *RPCError) RPCErrorDetails() (int, string, json.RawMessage) {
	if e == nil {
		return ProductFailure, "live RPC error", nil
	}
	return e.Code, e.Message, append(json.RawMessage(nil), e.Data...)
}

type Connection struct {
	connection net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan Frame
	report  Report
}

func NewConnection(connection net.Conn) *Connection {
	decoder := json.NewDecoder(bufio.NewReader(connection))
	decoder.DisallowUnknownFields()
	return &Connection{
		connection: connection, encoder: json.NewEncoder(connection), decoder: decoder,
		pending: map[string]chan Frame{},
	}
}

func (c *Connection) Decode(frame *Frame) error { return c.decoder.Decode(frame) }

func (c *Connection) Write(frame Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.encoder.Encode(frame)
}

func (c *Connection) Call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(method) == "" {
		return nil, errors.New("live RPC request identity is incomplete")
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	wireID, _ := json.Marshal(id)
	key := string(wireID)
	response := make(chan Frame, 1)
	c.mu.Lock()
	if _, duplicate := c.pending[key]; duplicate {
		c.mu.Unlock()
		return nil, errors.New("live RPC request id is already outstanding")
	}
	c.pending[key] = response
	c.mu.Unlock()
	if err := c.Write(Frame{JSONRPC: "2.0", ID: wireID, Method: method, Params: body}); err != nil {
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

func (c *Connection) drop(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Connection) Resolve(frame Frame) bool {
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

func (c *Connection) Fail() {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]chan Frame{}
	c.mu.Unlock()
	for _, response := range pending {
		close(response)
	}
}

func (c *Connection) Report() Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CloneReport(c.report)
}

func (c *Connection) SetReport(report Report) {
	c.mu.Lock()
	c.report = CloneReport(report)
	c.mu.Unlock()
}

func (c *Connection) UpdateReport(name string, info map[string]string) Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.report.Name, c.report.Info = name, CloneInfo(info)
	return CloneReport(c.report)
}

func (c *Connection) Close() error { return c.connection.Close() }

func ValidReport(report Report) bool {
	if !sessionidentity.ValidNativeID(report.UUID) || !sessionidentity.ValidName(report.Name) ||
		productcatalog.ValidateToken(report.Product) != nil || report.Groups == nil || report.Info == nil {
		return false
	}
	for _, group := range report.Groups {
		if !sessionidentity.ValidGroup(group) {
			return false
		}
	}
	return true
}

func CloneReport(report Report) Report {
	groups := make([]string, len(report.Groups))
	copy(groups, report.Groups)
	report.Groups = groups
	report.Info = CloneInfo(report.Info)
	return report
}

func CloneInfo(info map[string]string) map[string]string {
	cloned := make(map[string]string, len(info))
	for key, value := range info {
		cloned[key] = value
	}
	return cloned
}

func CwdInfo(cwd string) map[string]string {
	info := map[string]string{}
	if strings.TrimSpace(cwd) != "" {
		info["cwd"] = cwd
	}
	return info
}

func ValidRequest(frame Frame) bool {
	return frame.JSONRPC == "2.0" && validID(frame.ID) && strings.TrimSpace(frame.Method) != "" &&
		len(frame.Params) != 0 && json.Valid(frame.Params) && frame.Result == nil && frame.Error == nil
}

func ValidFrame(frame Frame) bool {
	if ValidRequest(frame) {
		return true
	}
	if frame.JSONRPC != "2.0" || !validID(frame.ID) || frame.Method != "" || frame.Params != nil {
		return false
	}
	return frame.Result != nil && frame.Error == nil || frame.Result == nil && validError(frame.Error)
}

func validID(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value := value.(type) {
	case string:
		return true
	case json.Number:
		return validSafeInteger(value)
	default:
		return false
	}
}

func IDText(raw json.RawMessage) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	return fmt.Sprint(value)
}

func validError(rpcErr *RPCError) bool {
	if rpcErr == nil || strings.TrimSpace(rpcErr.Message) == "" {
		return false
	}
	switch rpcErr.Code {
	case InvalidParams:
		return validStringError(rpcErr, "Invalid params", "method", validMethod)
	case Unknown:
		return validStringError(rpcErr, "Unknown session or target", "target", func(value string) bool { return strings.TrimSpace(value) != "" })
	case Busy:
		return validStringError(rpcErr, "Session busy", "uuid", sessionidentity.ValidNativeID)
	case NotPermitted:
		return rpcErr.Message == "Operation not permitted" && validNotPermittedData(rpcErr.Data)
	case UnsupportedVersion:
		var data struct {
			Supported *int         `json:"supported"`
			Received  *json.Number `json:"received"`
		}
		return rpcErr.Message == "Unsupported protocol version" && DecodeStrict(rpcErr.Data, &data) == nil &&
			data.Supported != nil && *data.Supported == ProtocolVersion && data.Received != nil && validSafeInteger(*data.Received)
	case ProductUnavailable:
		return validStringError(rpcErr, "Product not launchable", "product", func(value string) bool { return productcatalog.ValidateToken(value) == nil })
	case ProductFailure:
		return rpcErr.Data == nil || json.Valid(rpcErr.Data)
	default:
		return false
	}
}

func validStringError(rpcErr *RPCError, message, key string, validate func(string) bool) bool {
	value, ok := errorStringData(rpcErr.Data, key)
	return rpcErr.Message == message && ok && validate(value)
}

func errorStringData(raw json.RawMessage, key string) (string, bool) {
	data := map[string]string{}
	err := DecodeStrict(raw, &data)
	value, ok := data[key]
	return value, err == nil && len(data) == 1 && ok
}

func validNotPermittedData(raw json.RawMessage) bool {
	if method, ok := errorStringData(raw, "method"); ok {
		return validMethod(method)
	}
	if group, ok := errorStringData(raw, "group"); ok {
		return sessionidentity.ValidGroup(group)
	}
	reason, ok := errorStringData(raw, "reason")
	return ok && (reason == "no running turn" || reason == "steer unsupported")
}

func validMethod(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func validSafeInteger(value json.Number) bool {
	number, err := value.Float64()
	return err == nil && math.Trunc(number) == number && math.Abs(number) <= 9007199254740991
}

func DecodeHello(raw json.RawMessage) (Report, int, error) {
	var params struct {
		Protocol     *int               `json:"protocol"`
		UUID         *string            `json:"uuid"`
		Name         *string            `json:"name"`
		Groups       *[]string          `json:"groups"`
		Product      *string            `json:"product"`
		Info         *map[string]string `json:"info"`
		Capabilities *Capabilities      `json:"capabilities,omitempty"`
	}
	if err := DecodeStrict(raw, &params); err != nil || params.Protocol == nil || params.UUID == nil || params.Name == nil ||
		params.Groups == nil || params.Product == nil || params.Info == nil || params.Capabilities != nil && !params.Capabilities.Lane {
		return Report{}, 0, errors.New("session hello is invalid")
	}
	groups := make([]string, len(*params.Groups))
	copy(groups, *params.Groups)
	report := Report{
		UUID: *params.UUID, Name: *params.Name, Groups: groups,
		Product: *params.Product, Info: CloneInfo(*params.Info),
	}
	if params.Capabilities != nil {
		report.Capabilities = *params.Capabilities
	}
	if !ValidReport(report) {
		return Report{}, *params.Protocol, errors.New("session hello is invalid")
	}
	return report, *params.Protocol, nil
}

func DecodeUpdate(raw json.RawMessage) (string, map[string]string, error) {
	var params struct {
		Name *string            `json:"name"`
		Info *map[string]string `json:"info"`
	}
	if err := DecodeStrict(raw, &params); err != nil || params.Name == nil || params.Info == nil || !sessionidentity.ValidName(*params.Name) {
		return "", nil, errors.New("session update is invalid")
	}
	return *params.Name, CloneInfo(*params.Info), nil
}

func DecodeStrict(raw json.RawMessage, target any) error {
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

func Success(id, result json.RawMessage) Frame {
	if result == nil {
		result = json.RawMessage(`null`)
	}
	return Frame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Result: result}
}

func Failure(id json.RawMessage, code int, message string, data any) Frame {
	var encoded json.RawMessage
	if data != nil {
		encoded, _ = json.Marshal(data)
	}
	return Frame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Error: &RPCError{
		Code: code, Message: message, Data: encoded,
	}}
}

func NewError(code int, message string, data any) error {
	frame := Failure(json.RawMessage(`0`), code, message, data)
	return frame.Error
}

func FailureFromError(id json.RawMessage, method string, err error) Frame {
	var remote *RPCError
	if errors.As(err, &remote) && validError(remote) {
		return Frame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Error: remote}
	}
	var unknownTarget *federationpkg.UnknownTargetError
	if errors.As(err, &unknownTarget) {
		return Failure(id, Unknown, "Unknown session or target", map[string]any{"target": unknownTarget.Target})
	}
	var forbiddenGroup *federationpkg.GroupNotPermittedError
	if errors.As(err, &forbiddenGroup) {
		return Failure(id, NotPermitted, "Operation not permitted", map[string]any{"group": forbiddenGroup.Group})
	}
	if errors.Is(err, ErrInvalidParams) {
		return Failure(id, InvalidParams, "Invalid params", map[string]any{"method": method})
	}
	if errors.Is(err, ErrNoRunningTurn) {
		return Failure(id, NotPermitted, "Operation not permitted", map[string]any{"reason": "no running turn"})
	}
	if errors.Is(err, productruntime.ErrUnsupportedSteer) {
		return Failure(id, NotPermitted, "Operation not permitted", map[string]any{"reason": "steer unsupported"})
	}
	if errors.Is(err, ErrNotPermitted) || errors.Is(err, productruntime.ErrUnauthorized) ||
		errors.Is(err, productruntime.ErrUnsupportedPolicy) || errors.Is(err, productruntime.ErrUnsupportedRename) {
		return Failure(id, NotPermitted, "Operation not permitted", map[string]any{"method": method})
	}
	var classified classifiedError
	if errors.Is(err, ErrBusy) && errors.As(err, &classified) && sessionidentity.ValidNativeID(classified.uuid) {
		return Failure(id, Busy, "Session busy", map[string]any{"uuid": classified.uuid})
	}
	if (errors.Is(err, ErrProductUnavailable) || errors.Is(err, productruntime.ErrUnavailable) || errors.Is(err, productruntime.ErrIncompatible)) &&
		errors.As(err, &classified) && productcatalog.ValidateToken(classified.product) == nil {
		return Failure(id, ProductUnavailable, "Product not launchable", map[string]any{"product": classified.product})
	}
	return Failure(id, ProductFailure, err.Error(), map[string]any{
		"detail": err.Error(), "agent_sessions_bug_report": sessiontools.BugReportGuidance,
	})
}

type Client struct {
	ctx           context.Context
	endpoint      string
	report        Report
	resolveReport func(context.Context) (Report, bool)

	mu      sync.Mutex
	current *Connection
	call    func(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

func StartClient(
	ctx context.Context,
	endpoint string,
	report Report,
	call func(context.Context, string, json.RawMessage) (json.RawMessage, error),
) *Client {
	report = NormalizeReport(report)
	client := &Client{ctx: ctx, endpoint: endpoint, report: report, call: call}
	if ValidReport(report) {
		go client.run()
	}
	return client
}

func StartResolvingClient(
	ctx context.Context,
	endpoint string,
	resolveReport func(context.Context) (Report, bool),
	call func(context.Context, string, json.RawMessage) (json.RawMessage, error),
) *Client {
	client := &Client{ctx: ctx, endpoint: endpoint, resolveReport: resolveReport, call: call}
	go client.run()
	return client
}

func (c *Client) Call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
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
	return connection.Call(ctx, "session."+id, method, params)
}

func (c *Client) UpdateReport(ctx context.Context, report Report) error {
	report = NormalizeReport(report)
	if !ValidReport(report) {
		return errors.New("live session update is invalid")
	}
	c.mu.Lock()
	c.report = CloneReport(report)
	connection := c.current
	c.mu.Unlock()
	if connection == nil {
		return nil
	}
	_, err := connection.Call(ctx, "session.update", "session.update", map[string]any{"name": report.Name, "info": report.Info})
	return err
}

func NormalizeReport(report Report) Report {
	if report.Groups == nil {
		report.Groups = []string{}
	}
	if report.Info == nil {
		report.Info = map[string]string{}
	}
	return CloneReport(report)
}

func (c *Client) run() {
	for c.ctx.Err() == nil {
		connection, err := (&net.Dialer{}).DialContext(c.ctx, "unix", c.endpoint)
		if err == nil {
			c.mu.Lock()
			report := CloneReport(c.report)
			resolveReport := c.resolveReport
			c.mu.Unlock()
			confirmed := true
			if resolveReport != nil {
				report, confirmed = resolveReport(c.ctx)
			}
			report = NormalizeReport(report)
			if !confirmed || !ValidReport(report) {
				_ = connection.Close()
			} else {
				rpc := NewConnection(connection)
				closed := make(chan struct{})
				go func() {
					select {
					case <-c.ctx.Done():
						_ = connection.Close()
					case <-closed:
					}
				}()
				c.mu.Lock()
				c.report = CloneReport(report)
				err = hello(rpc, report)
				if err == nil {
					rpc.SetReport(report)
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
				rpc.Fail()
				_ = connection.Close()
				close(closed)
			}
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(ReconnectInterval):
		}
	}
}

func (c *Client) read(ctx context.Context, connection *Connection) {
	for {
		var frame Frame
		if connection.Decode(&frame) != nil {
			return
		}
		if !ValidFrame(frame) {
			return
		}
		if frame.Method == "" {
			if !connection.Resolve(frame) {
				return
			}
			continue
		}
		go func(frame Frame) {
			response := Success(frame.ID, nil)
			if !AllowsDaemonMethod(connection.Report(), frame.Method) {
				response = Failure(frame.ID, NotPermitted, "Operation not permitted", map[string]any{"method": frame.Method})
			} else if c.call == nil {
				response = Failure(frame.ID, NotPermitted, "Operation not permitted", map[string]any{"method": frame.Method})
			} else {
				result, err := c.call(ctx, frame.Method, frame.Params)
				if err != nil {
					response = FailureFromError(frame.ID, frame.Method, err)
				} else {
					response = Success(frame.ID, result)
				}
			}
			_ = connection.Write(response)
		}(frame)
	}
}

func AllowsDaemonMethod(report Report, method string) bool {
	return method == "message.deliver" || report.Capabilities.Lane && (method == "lane.turn.start" || method == "lane.turn.wait" || method == "lane.turn.interrupt" || method == "lane.session.archive")
}

func hello(connection *Connection, report Report) error {
	id := json.RawMessage(`"session.hello"`)
	hello := map[string]any{
		"protocol": ProtocolVersion, "uuid": report.UUID, "name": report.Name,
		"groups": report.Groups, "product": report.Product, "info": report.Info,
	}
	if report.Capabilities.Lane {
		hello["capabilities"] = report.Capabilities
	}
	params, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	if err := connection.Write(Frame{JSONRPC: "2.0", ID: id, Method: "session.hello", Params: params}); err != nil {
		return err
	}
	var response Frame
	if err := connection.Decode(&response); err != nil || !ValidFrame(response) || response.Method != "" || string(response.ID) != string(id) {
		return errors.New("live session hello returned an invalid response")
	}
	if response.Error != nil {
		return response.Error
	}
	return nil
}

func DecodeDeliver(params json.RawMessage) (productruntime.NativeMessage, error) {
	var request productruntime.NativeMessage
	if DecodeStrict(params, &request) != nil || strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.From.UUID) == "" ||
		strings.TrimSpace(request.From.Product) == "" || request.From.Groups == nil {
		return productruntime.NativeMessage{}, NewError(InvalidParams, "Invalid params", map[string]any{"method": "message.deliver"})
	}
	return request, nil
}
