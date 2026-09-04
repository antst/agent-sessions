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
	"path/filepath"
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
	MaxFrameBytes     = 1 << 20

	InvalidParams      = -32602
	Unknown            = -32001
	Busy               = -32002
	NotPermitted       = -32003
	UnsupportedVersion = -32004
	ProductUnavailable = -32005
	ProductFailure     = -32006
)

var (
	ErrFrameTooLarge      = errors.New("live frame exceeds 1 MiB")
	ErrFrameTruncated     = errors.New("live frame is not newline terminated")
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

type MethodDirection string

const (
	ClientToDaemon MethodDirection = "client-to-daemon"
	DaemonToClient MethodDirection = "daemon-to-client"
)

type MethodSpec struct {
	Direction        MethodDirection
	Lane, NeedsInput bool
	params, result   func(json.RawMessage) bool
}

type LaneCallParams struct {
	Product          string
	Arguments        []string
	Input, Cwd, Host *string
}

func LookupMethod(name string) (MethodSpec, bool) {
	spec := MethodSpec{Direction: ClientToDaemon}
	switch name {
	case "session.hello", "session.update", "peers.list", "message.send":
	case "lane.doctor", "lane.list", "lane.wait", "lane.status", "lane.interrupt", "lane.archive":
		spec.Lane = true
		if name == "lane.doctor" || name == "lane.list" {
			spec.result = map[string]func(json.RawMessage) bool{"lane.doctor": validDoctorResult, "lane.list": validListResult}[name]
		}
	case "lane.start", "lane.run", "lane.resume", "lane.steer":
		spec.Lane, spec.NeedsInput = true, true
	case "message.deliver":
		spec.Direction = DaemonToClient
	case "lane.turn.start", "lane.turn.wait", "lane.turn.interrupt", "lane.session.archive":
		spec.Direction, spec.Lane = DaemonToClient, true
		spec.params, spec.result = nativeValidators(name)
	default:
		return MethodSpec{}, false
	}
	if spec.Lane && spec.Direction == ClientToDaemon {
		spec.params = func(raw json.RawMessage) bool { _, ok := DecodeLaneCall(spec, raw); return ok }
	}
	return spec, true
}

func ValidMethodParams(spec MethodSpec, raw json.RawMessage) bool {
	return spec.params == nil || spec.params(raw)
}

func DecodeLaneCall(spec MethodSpec, raw json.RawMessage) (LaneCallParams, bool) {
	var p LaneCallParams
	if DecodeStrict(raw, &p) != nil || productcatalog.ValidateToken(p.Product) != nil || p.Arguments == nil || spec.NeedsInput != (p.Input != nil) || p.Input != nil && !textValue(*p.Input) || p.Cwd != nil && (*p.Cwd == "" || !filepath.IsAbs(*p.Cwd)) || p.Host != nil && !textValue(*p.Host) {
		return p, false
	}
	for _, argument := range p.Arguments {
		if strings.ContainsAny(argument, "\x00\r\n") {
			return p, false
		}
	}
	return p, true
}

func ValidMethodResult(spec MethodSpec, raw json.RawMessage) bool {
	return spec.result == nil || spec.result(raw)
}

func textValue(value string) bool { return strings.TrimSpace(value) != "" }
func nativeValidators(method string) (func(json.RawMessage) bool, func(json.RawMessage) bool) {
	return func(raw json.RawMessage) bool { return validNative(method, false, raw) }, func(raw json.RawMessage) bool { return validNative(method, true, raw) }
}
func validNative(method string, result bool, raw json.RawMessage) bool {
	if method == "lane.turn.interrupt" || method == "lane.session.archive" {
		_, ok := object(raw, "", "")
		return ok
	}
	if method == "lane.turn.start" && !result {
		v, ok := object(raw, "input_id body mode", "")
		input, inputOK := stringField(v, "input_id")
		body, bodyOK := stringField(v, "body")
		mode, modeOK := stringField(v, "mode")
		return ok && inputOK && textValue(input) && bodyOK && body != "" && modeOK && (mode == "followup" || mode == "steer")
	}
	if method == "lane.turn.start" || !result {
		v, ok := object(raw, "native_message_id", "")
		value, textOK := stringField(v, "native_message_id")
		return ok && textOK && textValue(value)
	}
	v, ok := object(raw, "outcome result reason", "")
	outcome, outcomeOK := stringField(v, "outcome")
	_, resultOK := stringField(v, "result")
	return ok && outcomeOK && resultOK && (outcome == "completed" || outcome == "interrupted" || outcome == "failed")
}
func object(raw json.RawMessage, required, optional string) (map[string]any, bool) {
	var value map[string]any
	return value, DecodeStrict(raw, &value) == nil && exactObject(value, required, optional)
}
func exactObject(value map[string]any, required, optional string) bool {
	if value == nil || len(value) < len(strings.Fields(required)) {
		return false
	}
	allowed := " " + required + " " + optional + " "
	for key := range value {
		if !strings.Contains(allowed, " "+key+" ") {
			return false
		}
	}
	for _, key := range strings.Fields(required) {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}
func stringField(value map[string]any, key string) (string, bool) {
	item, ok := value[key].(string)
	return item, ok
}

func validDoctorResult(raw json.RawMessage) bool {
	var probe map[string]any
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	product, ok := stringField(probe, "product")
	if !ok {
		return false
	}
	value, ok := object(raw, "type contract_version authority product ready native_path runtime_path daemon_reachable supervisor_reachable "+product+"_available "+product+"_path "+product+"_version", product+"_error readiness_error")
	if !ok || value["type"] != "lane.doctor" || value["authority"] != "daemon" || value["contract_version"] != float64(2) || productcatalog.ValidateToken(product) != nil {
		return false
	}
	for _, key := range []string{"ready", "daemon_reachable", "supervisor_reachable", product + "_available"} {
		if _, ok := value[key].(bool); !ok {
			return false
		}
	}
	for _, key := range []string{"native_path", "runtime_path", product + "_path", product + "_version", product + "_error", "readiness_error"} {
		if item, present := value[key]; present {
			if _, ok := item.(string); !ok {
				return false
			}
		}
	}
	return true
}
func validListResult(raw json.RawMessage) bool {
	value, ok := object(raw, "type product lanes", "")
	product, productOK := stringField(value, "product")
	lanes, lanesOK := value["lanes"].([]any)
	if !ok || value["type"] != "lane.list" || !productOK || productcatalog.ValidateToken(product) != nil || !lanesOK {
		return false
	}
	for _, item := range lanes {
		lane, mapOK := item.(map[string]any)
		if !mapOK || !exactObject(lane, "type product session_id name cwd groups permission_mode state turn_id outcome exit owner_session_id persistent auto_archive auto_archive_after_seconds auto_archive_at", "") || lane["type"] != "lane.status" || lane["product"] != product || !laneTypes(lane) {
			return false
		}
	}
	return true
}
func laneTypes(lane map[string]any) bool {
	for _, key := range strings.Fields("session_id name cwd permission_mode state turn_id outcome owner_session_id") {
		if _, ok := lane[key].(string); !ok {
			return false
		}
	}
	groups, ok := lane["groups"].([]any)
	if !ok {
		return false
	}
	for _, group := range groups {
		if _, ok := group.(string); !ok {
			return false
		}
	}
	for _, key := range []string{"persistent", "auto_archive"} {
		if _, ok := lane[key].(bool); !ok {
			return false
		}
	}
	if _, ok := lane["auto_archive_after_seconds"].(float64); !ok {
		return false
	}
	autoAt, ok := lane["auto_archive_at"].(float64)
	if !ok || math.Trunc(autoAt) != autoAt {
		return false
	}
	exit, ok := lane["exit"].(float64)
	return lane["exit"] == nil || ok && math.Trunc(exit) == exit
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
	reader     *bufio.Reader
	observer   func(string, Frame)

	writeMu   sync.Mutex
	observeMu sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan Frame
	report    Report
}

func NewConnection(connection net.Conn) *Connection {
	return &Connection{
		connection: connection, reader: bufio.NewReaderSize(connection, MaxFrameBytes+1),
		pending: map[string]chan Frame{},
	}
}
func (c *Connection) Observe(observer func(string, Frame)) { c.observer = observer }
func (c *Connection) Decode(frame *Frame) error {
	body, err := c.reader.ReadSlice('\n')
	if err != nil && len(body) == 0 {
		return err
	}
	if errors.Is(err, bufio.ErrBufferFull) || len(body) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if err != nil {
		return ErrFrameTruncated
	}
	if err := DecodeStrict(body[:len(body)-1], frame); err != nil {
		return err
	}
	c.observe("receive", *frame)
	return nil
}
func (c *Connection) Write(frame Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	c.observe("send", frame)
	_, err = io.Copy(c.connection, bytes.NewReader(body))
	return err
}
func (c *Connection) observe(direction string, frame Frame) {
	if c.observer != nil {
		c.observeMu.Lock()
		defer c.observeMu.Unlock()
		c.observer(direction, cloneFrame(frame))
	}
}
func cloneFrame(frame Frame) Frame {
	body, _ := json.Marshal(frame)
	var clone Frame
	_ = json.Unmarshal(body, &clone)
	return clone
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
		productcatalog.ValidateToken(report.Product) != nil || report.Groups == nil || !validInfo(report.Info) {
		return false
	}
	for _, group := range report.Groups {
		if !sessionidentity.ValidGroup(group) {
			return false
		}
	}
	return true
}

func validInfo(info map[string]string) bool {
	cwd, present := info["cwd"]
	return info != nil && (!present || cwd != "" && filepath.IsAbs(cwd))
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
	if err := DecodeStrict(raw, &params); err != nil || params.Name == nil || params.Info == nil ||
		!sessionidentity.ValidName(*params.Name) || !validInfo(*params.Info) {
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
	observer      func(string, Frame)
	ready         chan struct{}
	done          chan struct{}
	readyOnce     sync.Once

	mu      sync.Mutex
	current *Connection
	call    func(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

func StartClient(
	ctx context.Context,
	endpoint string,
	report Report,
	call func(context.Context, string, json.RawMessage) (json.RawMessage, error),
	observers ...func(string, Frame),
) *Client {
	report = NormalizeReport(report)
	client := initializeClient(&Client{ctx: ctx, endpoint: endpoint, report: report, call: call}, observers)
	if ValidReport(report) {
		go client.run()
	} else {
		close(client.done)
	}
	return client
}

func StartResolvingClient(
	ctx context.Context,
	endpoint string,
	resolveReport func(context.Context) (Report, bool),
	call func(context.Context, string, json.RawMessage) (json.RawMessage, error),
	observers ...func(string, Frame),
) *Client {
	client := initializeClient(&Client{ctx: ctx, endpoint: endpoint, resolveReport: resolveReport, call: call}, observers)
	go client.run()
	return client
}

func initializeClient(client *Client, observers []func(string, Frame)) *Client {
	client.ready, client.done = make(chan struct{}), make(chan struct{})
	if len(observers) != 0 {
		client.observer = observers[0]
	}
	return client
}
func (c *Client) Ready() <-chan struct{} { return c.ready }
func (c *Client) Done() <-chan struct{}  { return c.done }

func (c *Client) Call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	spec, known := LookupMethod(method)
	body, marshalErr := json.Marshal(params)
	if !known || spec.Direction != ClientToDaemon {
		return nil, NewError(NotPermitted, "Operation not permitted", map[string]any{"method": method})
	}
	if marshalErr != nil || !ValidMethodParams(spec, body) {
		return nil, NewError(InvalidParams, "Invalid params", map[string]any{"method": method})
	}
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
	result, err := connection.Call(ctx, "session."+id, method, params)
	if err == nil && !ValidMethodResult(spec, result) {
		return nil, errors.New("live presence method returned an invalid result")
	}
	return result, err
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
	defer close(c.done)
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
				rpc.Observe(c.observer)
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
					c.readyOnce.Do(func() { close(c.ready) })
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
			spec, known := LookupMethod(frame.Method)
			if !known || spec.Direction != DaemonToClient || spec.Lane && !connection.Report().Capabilities.Lane {
				response = Failure(frame.ID, NotPermitted, "Operation not permitted", map[string]any{"method": frame.Method})
			} else if !ValidMethodParams(spec, frame.Params) {
				response = Failure(frame.ID, InvalidParams, "Invalid params", map[string]any{"method": frame.Method})
			} else if c.call == nil {
				response = Failure(frame.ID, NotPermitted, "Operation not permitted", map[string]any{"method": frame.Method})
			} else {
				result, err := c.call(ctx, frame.Method, frame.Params)
				if err != nil {
					response = FailureFromError(frame.ID, frame.Method, err)
				} else if !ValidMethodResult(spec, result) {
					response = FailureFromError(frame.ID, frame.Method, errors.New("product returned an invalid native lane result"))
				} else {
					response = Success(frame.ID, result)
				}
			}
			_ = connection.Write(response)
		}(frame)
	}
}

func AllowsDaemonMethod(report Report, method string) bool {
	spec, known := LookupMethod(method)
	return known && spec.Direction == DaemonToClient && (!spec.Lane || report.Capabilities.Lane)
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
