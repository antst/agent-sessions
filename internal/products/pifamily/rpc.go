package pifamily

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/structuredprocess"
)

// RPCProcess is the smallest structuredprocess surface used by the family RPC
// client. Production factories below always return an exactly owned child.
type RPCProcess interface {
	ReadFrame(context.Context) ([]byte, error)
	WriteFrame(context.Context, []byte) error
	Signal(context.Context, productruntime.ProcessSignal) error
	Wait(context.Context) (productruntime.ProcessExit, error)
	Cleanup(context.Context) error
}

// ProcessFactory starts one private JSONL RPC child.
type ProcessFactory interface {
	StartRPC(context.Context, productruntime.NativeCommand) (RPCProcess, error)
}

type structuredFactory struct{ supervisor *structuredprocess.Supervisor }

// NewStructuredProcessFactory adapts the shared owned-child engine without
// reimplementing process groups, cancellation, or framing.
func NewStructuredProcessFactory(supervisor *structuredprocess.Supervisor) (ProcessFactory, error) {
	if supervisor == nil {
		return nil, errors.New("Pi-family structured process supervisor is nil")
	}
	return structuredFactory{supervisor: supervisor}, nil
}

func (factory structuredFactory) StartRPC(ctx context.Context, command productruntime.NativeCommand) (RPCProcess, error) {
	return factory.supervisor.StartProcess(ctx, command)
}

type rpcResponse struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
	Code    string          `json:"code"`
}

type rpcSessionState struct {
	SessionID    string `json:"sessionId"`
	SessionName  string `json:"sessionName"`
	SessionFile  string `json:"sessionFile"`
	IsStreaming  bool   `json:"isStreaming"`
	IsCompacting bool   `json:"isCompacting"`
}

type rpcTerminal struct {
	Type         string            `json:"type"`
	Messages     []json.RawMessage `json:"messages"`
	WillRetry    bool              `json:"willRetry"`
	WillContinue bool              `json:"willContinue"`
}

type pendingRPC struct {
	command string
	result  chan rpcResult
}

type rpcResult struct {
	response rpcResponse
	err      error
}

type rpcClient struct {
	quirks  Quirks
	process RPCProcess

	requestCounter atomic.Uint64
	mu             sync.Mutex
	pending        map[string]pendingRPC
	failed         error
	ready          chan struct{}
	readyOnce      sync.Once
	terminal       chan rpcTerminal
	done           chan struct{}
	doneOnce       sync.Once
}

func newRPCClient(process RPCProcess, quirks Quirks) (*rpcClient, error) {
	if process == nil {
		return nil, errors.New("Pi-family RPC process is nil")
	}
	if err := quirks.Validate(); err != nil {
		return nil, err
	}
	client := &rpcClient{
		quirks: quirks, process: process, pending: make(map[string]pendingRPC),
		ready: make(chan struct{}), terminal: make(chan rpcTerminal, 8), done: make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

func (client *rpcClient) handshake(ctx context.Context) (rpcSessionState, error) {
	if client.quirks.ReadyStrategy == ReadyByEvent {
		select {
		case <-ctx.Done():
			return rpcSessionState{}, ctx.Err()
		case <-client.done:
			return rpcSessionState{}, client.failure()
		case <-client.ready:
		}
	}
	state, err := client.getState(ctx)
	if err != nil {
		return rpcSessionState{}, err
	}
	if state.SessionID == "" {
		return rpcSessionState{}, fmt.Errorf("%w: get_state omitted the native session id", productruntime.ErrProtocol)
	}
	if state.IsStreaming || state.IsCompacting {
		return rpcSessionState{}, fmt.Errorf("%w: native session was not idle at RPC adoption", productruntime.ErrAmbiguousSession)
	}
	return state, nil
}

func (client *rpcClient) getState(ctx context.Context) (rpcSessionState, error) {
	response, err := client.call(ctx, "get_state", nil)
	if err != nil {
		return rpcSessionState{}, err
	}
	var state rpcSessionState
	if len(response.Data) == 0 || json.Unmarshal(response.Data, &state) != nil {
		return rpcSessionState{}, fmt.Errorf("%w: get_state response has invalid data", productruntime.ErrProtocol)
	}
	return state, nil
}

func (client *rpcClient) lastAssistantText(ctx context.Context) (string, error) {
	response, err := client.call(ctx, "get_last_assistant_text", nil)
	if err != nil {
		return "", err
	}
	var data struct {
		Text *string `json:"text"`
	}
	if len(response.Data) == 0 || json.Unmarshal(response.Data, &data) != nil {
		return "", fmt.Errorf("%w: get_last_assistant_text response has invalid data", productruntime.ErrProtocol)
	}
	if data.Text == nil {
		return "", nil
	}
	return *data.Text, nil
}

func (client *rpcClient) call(ctx context.Context, command string, fields map[string]any) (rpcResponse, error) {
	if ctx == nil {
		return rpcResponse{}, errors.New("Pi-family RPC call requires context")
	}
	if err := ctx.Err(); err != nil {
		return rpcResponse{}, err
	}
	id := fmt.Sprintf("as-%d", client.requestCounter.Add(1))
	request := make(map[string]any, len(fields)+2)
	request["id"] = id
	request["type"] = command
	for key, value := range fields {
		if key == "id" || key == "type" {
			return rpcResponse{}, fmt.Errorf("%w: RPC request attempted to override %s", productruntime.ErrProtocol, key)
		}
		request[key] = value
	}
	body, err := json.Marshal(request)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("%w: encode %s request: %v", productruntime.ErrProtocol, command, err)
	}
	if len(body) == 0 || len(body) > MaxRPCFrameBytes {
		return rpcResponse{}, fmt.Errorf("%w: RPC request exceeds frame bound", productruntime.ErrProtocol)
	}
	pending := pendingRPC{command: command, result: make(chan rpcResult, 1)}
	client.mu.Lock()
	if client.failed != nil {
		err = client.failed
	} else {
		client.pending[id] = pending
	}
	client.mu.Unlock()
	if err != nil {
		return rpcResponse{}, err
	}
	if err := client.process.WriteFrame(ctx, body); err != nil {
		client.removePending(id)
		return rpcResponse{}, fmt.Errorf("%w: write %s request: %v", productruntime.ErrProtocol, command, err)
	}
	select {
	case <-ctx.Done():
		client.removePending(id)
		return rpcResponse{}, ctx.Err()
	case result := <-pending.result:
		return result.response, result.err
	case <-client.done:
		client.removePending(id)
		return rpcResponse{}, client.failure()
	}
}

func (client *rpcClient) waitTerminal(ctx context.Context) (rpcTerminal, error) {
	select {
	case <-ctx.Done():
		return rpcTerminal{}, ctx.Err()
	case terminal := <-client.terminal:
		return terminal, nil
	case <-client.done:
		return rpcTerminal{}, client.failure()
	}
}

func (client *rpcClient) readLoop() {
	readyObserved := false
	for {
		body, err := client.process.ReadFrame(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = fmt.Errorf("%w: RPC stdout closed", productruntime.ErrUnavailable)
			} else {
				err = fmt.Errorf("%w: read RPC frame: %v", productruntime.ErrProtocol, err)
			}
			client.fail(err)
			return
		}
		if len(body) == 0 || len(body) > MaxRPCFrameBytes {
			client.fail(fmt.Errorf("%w: RPC frame exceeds bound", productruntime.ErrProtocol))
			return
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(body, &envelope) != nil || envelope.Type == "" {
			client.fail(fmt.Errorf("%w: RPC frame is not a typed JSON object", productruntime.ErrProtocol))
			return
		}
		if client.quirks.ReadyStrategy == ReadyByEvent && !readyObserved {
			if envelope.Type != "ready" {
				client.fail(fmt.Errorf("%w: OMP emitted %q before ready", productruntime.ErrProtocol, envelope.Type))
				return
			}
			if err := validateReady(body); err != nil {
				client.fail(err)
				return
			}
			readyObserved = true
			client.readyOnce.Do(func() { close(client.ready) })
			continue
		}
		switch envelope.Type {
		case "ready":
			client.fail(fmt.Errorf("%w: duplicate or unsupported ready frame", productruntime.ErrProtocol))
			return
		case "response":
			if err := client.handleResponse(body); err != nil {
				client.fail(err)
				return
			}
		case client.quirks.TerminalEvent:
			var terminal rpcTerminal
			if json.Unmarshal(body, &terminal) != nil {
				client.fail(fmt.Errorf("%w: malformed terminal event", productruntime.ErrProtocol))
				return
			}
			if terminal.WillContinue {
				continue
			}
			select {
			case client.terminal <- terminal:
			default:
				client.fail(fmt.Errorf("%w: terminal event queue exceeded bound", productruntime.ErrProtocol))
				return
			}
		case "extension_ui_request":
			// The pinned OMP RPC protocol uses this frame for interactive
			// approval. Agent Sessions has no host permission-authority callback
			// in this driver contract, so treating it as additive output would
			// leave an unattended turn blocked forever. Fail closed promptly.
			client.fail(fmt.Errorf("%w: native RPC approval mediation is unavailable", productruntime.ErrUnsupportedPolicy))
			return
		default:
			// Streaming events are product output, not control authority. Unknown
			// additive events are ignored at this pinned protocol version.
		}
	}
}

func validateReady(body []byte) error {
	var ready struct {
		ProtocolVersion           int   `json:"protocolVersion"`
		SupportedProtocolVersions []int `json:"supportedProtocolVersions"`
		MaxFrameBytes             int   `json:"maxFrameBytes"`
	}
	if json.Unmarshal(body, &ready) != nil || ready.ProtocolVersion != 1 || ready.MaxFrameBytes != MaxRPCFrameBytes {
		return fmt.Errorf("%w: OMP ready frame is incompatible", productruntime.ErrIncompatible)
	}
	for _, version := range ready.SupportedProtocolVersions {
		if version == 1 {
			return nil
		}
	}
	return fmt.Errorf("%w: OMP ready frame does not advertise protocol 1", productruntime.ErrIncompatible)
}

func (client *rpcClient) handleResponse(body []byte) error {
	var response rpcResponse
	if json.Unmarshal(body, &response) != nil || response.ID == "" || response.Command == "" {
		return fmt.Errorf("%w: malformed correlated RPC response", productruntime.ErrProtocol)
	}
	client.mu.Lock()
	pending, ok := client.pending[response.ID]
	if ok {
		delete(client.pending, response.ID)
	}
	client.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: RPC response names an unknown request", productruntime.ErrProtocol)
	}
	if response.Command != pending.command {
		return fmt.Errorf("%w: RPC response command mismatch", productruntime.ErrProtocol)
	}
	if !response.Success {
		detail := productruntime.NewRedactedString(response.Error)
		pending.result <- rpcResult{err: fmt.Errorf("%w: %s", productruntime.ErrNativeRejected, detail.String())}
		return nil
	}
	pending.result <- rpcResult{response: response}
	return nil
}

func (client *rpcClient) removePending(id string) {
	client.mu.Lock()
	delete(client.pending, id)
	client.mu.Unlock()
}

func (client *rpcClient) fail(err error) {
	client.doneOnce.Do(func() {
		client.mu.Lock()
		client.failed = err
		pending := client.pending
		client.pending = make(map[string]pendingRPC)
		client.mu.Unlock()
		for _, operation := range pending {
			operation.result <- rpcResult{err: err}
		}
		close(client.done)
	})
}

func (client *rpcClient) failure() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.failed != nil {
		return client.failed
	}
	return fmt.Errorf("%w: RPC client stopped", productruntime.ErrUnavailable)
}
