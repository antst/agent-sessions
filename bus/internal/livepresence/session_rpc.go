package livepresence

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"strconv"
	"sync"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

const (
	SessionInvalidFrame = -32600
	SessionInternal     = -32603
	SessionBusy         = -32003
	SessionNotRunning   = -32004
	SessionSpawnFailed  = -32009
)

type SessionExtraArgument struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	TakesValue  bool   `json:"takes_value"`
}

type SessionHello struct {
	Product             string                 `json:"product"`
	Version             string                 `json:"version,omitempty"`
	SupportedOpenFields []string               `json:"supported_open_fields"`
	ExtraArguments      []SessionExtraArgument `json:"extra_arguments"`
}

type SessionOpenOptions struct {
	Cwd             string   `json:"cwd,omitempty"`
	PermissionMode  string   `json:"permission_mode,omitempty"`
	Model           string   `json:"model,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Arguments       []string `json:"arguments,omitempty"`
}

type SessionOpenRequest struct {
	Name            string             `json:"name"`
	Groups          []string           `json:"groups"`
	ResumeSessionID string             `json:"resume_session_id,omitempty"`
	Open            SessionOpenOptions `json:"open"`
}

type SessionOpenResult struct {
	SessionID string `json:"session_id"`
}

type SessionTurnResult struct {
	Outcome          string `json:"outcome"`
	Result           string `json:"result"`
	Truncated        bool   `json:"truncated,omitempty"`
	NativeStopReason string `json:"native_stop_reason,omitempty"`
}

type SessionDeliverySource struct {
	SessionID string   `json:"session_id"`
	Name      string   `json:"name"`
	Product   string   `json:"product"`
	Groups    []string `json:"groups"`
}

type SessionDeliveryRequest struct {
	MessageID string                `json:"message_id"`
	From      SessionDeliverySource `json:"from"`
	Body      string                `json:"body"`
}

type SessionDeliveryReceipt struct {
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

type SessionMethod struct {
	Params string
	Result string
	Client bool
	Daemon bool
}

var SessionMethods = map[string]SessionMethod{
	"session.hello":      {"SessionHelloRequest", "SessionHelloResult", true, false},
	"session.superseded": {"SessionSupersededRequest", "SessionSupersededResult", false, true},
	"session.list":       {"SessionListRequest", "SessionListResult", true, false},
	"message.send":       {"MessageSendRequest", "MessageSendResult", true, false},
	"message.deliver":    {"MessageDeliverRequest", "MessageDeliverResult", false, true},
	"lane.describe":      {"LaneDescribeRequest", "LaneDescribeResult", true, false},
	"lane.spawn":         {"LaneSpawnRequest", "LaneSpawnResult", true, false},
	"session.open":       {"SessionOpenRequest", "SessionOpenResult", false, true},
	"turn.run":           {"TurnRunRequest", "TurnRunResult", true, true},
	"turn.interrupt":     {"TurnInterruptRequest", "TurnInterruptResult", true, true},
	"session.close":      {"SessionCloseRequest", "SessionCloseResult", true, true},
}

type sessionPending struct {
	result   string
	response chan Frame
}

// SessionRPC owns generic framing, validation, and full-duplex request matching.
// It deliberately knows nothing about session lifecycle or product behavior.
type SessionRPC struct {
	wire   *Connection
	schema *SessionSchema

	mu              sync.Mutex
	next            int64
	closed          bool
	closeDone       chan struct{}
	closeErr        error
	pending         map[string]sessionPending
	requests        map[string]bool
	seen            map[string]bool
	unmatched       sync.Once
	testBeforeWrite func()
}

func NewSessionRPC(connection net.Conn) (*SessionRPC, error) {
	schema, err := CompileSessionSchema(protocol.SessionSchema)
	if err != nil {
		return nil, err
	}
	return &SessionRPC{
		wire:      NewConnection(connection),
		schema:    schema,
		pending:   make(map[string]sessionPending),
		requests:  make(map[string]bool),
		seen:      make(map[string]bool),
		closeDone: make(chan struct{}),
	}, nil
}

func (r *SessionRPC) Observe(observer func(string, Frame)) { r.wire.Observe(observer) }

// Call sends a request. client says whether the local endpoint acts in the
// client-to-daemon direction for this call.
func (r *SessionRPC) Call(ctx context.Context, client bool, method string, params, result any) error {
	spec, ok := SessionMethods[method]
	if !ok || client && !spec.Client || !client && !spec.Daemon {
		return fmt.Errorf("session method %q has the wrong direction", method)
	}
	body, err := json.Marshal(params)
	if err != nil || r.schema.ValidateJSON(spec.Params, body) != nil {
		return errors.New("invalid session method params")
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("session connection closed")
	}
	r.next++
	if r.next > 1<<53-1 {
		r.mu.Unlock()
		return errors.New("session request id space exhausted")
	}
	id := json.RawMessage(strconv.FormatInt(r.next, 10))
	key := string(id)
	response := make(chan Frame, 1)
	r.pending[key] = sessionPending{result: spec.Result, response: response}
	r.mu.Unlock()

	if r.testBeforeWrite != nil {
		r.testBeforeWrite()
	}
	r.mu.Lock()
	if r.closed {
		delete(r.pending, key)
		r.mu.Unlock()
		return errors.New("session connection closed")
	}
	err = r.wire.Write(Frame{JSONRPC: "2.0", ID: id, Method: method, Params: body})
	if err != nil {
		delete(r.pending, key)
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		r.abandon(key, response, false)
		return ctx.Err()
	case frame, open := <-response:
		if !open {
			return errors.New("session connection closed")
		}
		if frame.Error != nil {
			return frame.Error
		}
		if result != nil {
			return json.Unmarshal(frame.Result, result)
		}
		return nil
	}
}

// Read returns the next validated inbound request. daemon says whether the
// remote endpoint is acting in the daemon-to-client direction.
func (r *SessionRPC) Read(daemon bool) (Frame, error) {
	for {
		var frame Frame
		if err := r.decode(&frame); err != nil {
			if _, recoverable := sessionNumericID(frame.ID); recoverable && frame.Method != "" {
				if writeErr := r.reject(frame); writeErr == nil {
					continue
				} else {
					return Frame{}, writeErr
				}
			}
			return Frame{}, err
		}
		key, numeric := sessionNumericID(frame.ID)
		if frame.Method == "" {
			if !numeric {
				return Frame{}, errors.New("invalid session response")
			}
			if !validSessionResponse(frame) {
				return Frame{}, errors.New("invalid session response")
			}
			if !r.resolve(key, frame) {
				r.unmatched.Do(func() { log.Printf("agentbus: dropping unmatched session response id %s", key) })
			}
			continue
		}
		spec, known := SessionMethods[frame.Method]
		valid := numeric && ValidRequest(frame) && known &&
			((daemon && spec.Daemon) || (!daemon && spec.Client)) &&
			r.schema.ValidateJSON(spec.Params, frame.Params) == nil
		if !valid {
			if numeric {
				if err := r.reject(frame); err != nil {
					return Frame{}, err
				}
				continue
			}
			return Frame{}, errors.New("invalid session request")
		}
		r.mu.Lock()
		duplicate := r.closed || r.seen[key]
		if !duplicate {
			r.seen[key] = true
			r.requests[key] = true
		}
		r.mu.Unlock()
		if duplicate {
			_ = r.reject(frame)
			continue
		}
		return frame, nil
	}
}

func (r *SessionRPC) Result(request Frame, result any) error {
	spec, ok := SessionMethods[request.Method]
	if !ok {
		return errors.New("unknown session method")
	}
	body, err := json.Marshal(result)
	if err != nil || r.schema.ValidateJSON(spec.Result, body) != nil {
		return errors.New("invalid session method result")
	}
	return r.finish(request, Success(request.ID, body))
}

func (r *SessionRPC) Error(request Frame, code int, message string, data any) error {
	response := Failure(request.ID, code, message, data)
	body, _ := json.Marshal(response.Error)
	if r.schema.ValidateJSON("RPCError", body) != nil {
		return errors.New("invalid session RPC error")
	}
	return r.finish(request, response)
}

func (r *SessionRPC) finish(request, response Frame) error {
	r.mu.Lock()
	if r.closed || !r.requests[string(request.ID)] {
		r.mu.Unlock()
		return errors.New("session request is no longer pending")
	}
	delete(r.requests, string(request.ID))
	err := r.wire.Write(response)
	r.mu.Unlock()
	return err
}

func (r *SessionRPC) reject(request Frame) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrConnectionClosed
	}
	err := r.wire.Write(Failure(request.ID, SessionInvalidFrame, "invalid_frame", nil))
	r.mu.Unlock()
	return err
}

func (r *SessionRPC) abandon(key string, response chan Frame, drop bool) {
	r.mu.Lock()
	pending, ok := r.pending[key]
	if ok && pending.response == response {
		if drop {
			delete(r.pending, key)
		} else {
			pending.response = nil
			r.pending[key] = pending
		}
	}
	r.mu.Unlock()
}

func (r *SessionRPC) resolve(key string, frame Frame) bool {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	pending, ok := r.pending[key]
	if ok {
		delete(r.pending, key)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	valid := false
	if frame.Error != nil {
		body, _ := json.Marshal(frame.Error)
		valid = r.schema.ValidateJSON("RPCError", body) == nil
	} else {
		valid = r.schema.ValidateJSON(pending.result, frame.Result) == nil
	}
	if pending.response == nil {
		return valid
	}
	if !valid {
		close(pending.response)
		return false
	}
	pending.response <- frame
	close(pending.response)
	return true
}

func (r *SessionRPC) Close() error {
	r.mu.Lock()
	if r.closed {
		done := r.closeDone
		r.mu.Unlock()
		<-done
		return r.closeErr
	}
	r.closed = true
	r.mu.Unlock()
	err := r.wire.Close()
	r.mu.Lock()
	pending := r.pending
	r.pending = make(map[string]sessionPending)
	r.requests = make(map[string]bool)
	r.closeErr = err
	r.mu.Unlock()
	for _, call := range pending {
		if call.response != nil {
			close(call.response)
		}
	}
	r.mu.Lock()
	close(r.closeDone)
	r.mu.Unlock()
	return err
}

func (r *SessionRPC) decode(frame *Frame) error {
	body, err := r.wire.reader.ReadSlice('\n')
	if err != nil && len(body) == 0 {
		return err
	}
	if errors.Is(err, bufio.ErrBufferFull) || len(body) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if err != nil {
		return ErrFrameTruncated
	}
	body = body[:len(body)-1]
	if err := DecodeStrict(body, frame); err != nil {
		var header struct {
			ID     json.RawMessage `json:"id"`
			Method json.RawMessage `json:"method"`
		}
		if json.Unmarshal(body, &header) == nil {
			frame.ID = header.ID
			if len(header.Method) != 0 && !bytes.Equal(bytes.TrimSpace(header.Method), []byte("null")) {
				_ = json.Unmarshal(header.Method, &frame.Method)
			}
		}
		return err
	}
	r.wire.observe("receive", *frame)
	return nil
}

func validSessionResponse(frame Frame) bool {
	if frame.JSONRPC != "2.0" || frame.Method != "" || frame.Params != nil || frame.methodNull || frame.errorNull {
		return false
	}
	return frame.Result != nil && frame.Error == nil || frame.Result == nil && frame.Error != nil
}

func sessionNumericID(raw json.RawMessage) (string, bool) {
	var number json.Number
	if json.Unmarshal(raw, &number) != nil {
		return "", false
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	valid := err == nil && value >= 1 && value <= 1<<53-1 && math.Trunc(value) == value
	if !valid {
		return "", false
	}
	return strconv.FormatInt(int64(value), 10), true
}
