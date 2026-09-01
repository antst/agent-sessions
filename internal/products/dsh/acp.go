package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/structuredprocess"
)

const (
	ACPProtocolVersion = 1
	maxACPFrameBytes   = 1 << 20
)

// ACPProcess is the narrow framed child surface used by the typed DSH ACP
// client. structuredprocess.Process satisfies it directly.
type ACPProcess interface {
	Ref() productruntime.OwnedProcessRef
	ReadFrame(context.Context) ([]byte, error)
	WriteFrame(context.Context, []byte) error
	Cleanup(context.Context) error
	Wait(context.Context) (productruntime.ProcessExit, error)
}

type ACPProcessFactory interface {
	StartACPProcess(context.Context, productruntime.NativeCommand) (ACPProcess, error)
}

type ACPProcessFactoryFunc func(context.Context, productruntime.NativeCommand) (ACPProcess, error)

func (function ACPProcessFactoryFunc) StartACPProcess(ctx context.Context, command productruntime.NativeCommand) (ACPProcess, error) {
	return function(ctx, command)
}

type StructuredProcessFactory struct{ Supervisor *structuredprocess.Supervisor }

func (factory StructuredProcessFactory) StartACPProcess(ctx context.Context, command productruntime.NativeCommand) (ACPProcess, error) {
	if factory.Supervisor == nil {
		return nil, fmt.Errorf("%w: DSH structured-process supervisor is unavailable", productruntime.ErrUnavailable)
	}
	return factory.Supervisor.StartProcess(ctx, command)
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (failure *rpcError) Error() string {
	if failure == nil {
		return ""
	}
	return fmt.Sprintf("DSH ACP %d: %s", failure.Code, productruntime.NewRedactedString(failure.Message))
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type possibleACPWriteError struct{ err error }

func (failure *possibleACPWriteError) Error() string { return failure.err.Error() }
func (failure *possibleACPWriteError) Unwrap() error { return failure.err }

type rpcFuture struct {
	id     string
	method string
	done   <-chan rpcResult
}

type acpNotification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type acpServerRequest struct {
	Method string
	Params json.RawMessage
}

type acpServerRequestHandler func(acpServerRequest) (any, *rpcError)

type ACPClient struct {
	process ACPProcess

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan rpcResult
	closed  error
	notify  func(acpNotification)
	request acpServerRequestHandler
}

func NewACPClient(process ACPProcess, notify func(acpNotification), handlers ...acpServerRequestHandler) (*ACPClient, error) {
	if process == nil {
		return nil, errors.New("DSH ACP client requires an owned process")
	}
	if len(handlers) > 1 {
		return nil, errors.New("DSH ACP client accepts at most one server-request handler")
	}
	client := &ACPClient{process: process, pending: make(map[string]chan rpcResult), notify: notify}
	if len(handlers) == 1 {
		client.request = handlers[0]
	}
	go client.readLoop()
	return client, nil
}

func (client *ACPClient) Initialize(ctx context.Context) error {
	var response struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	err := client.Request(ctx, "initialize", map[string]any{
		"protocolVersion":    ACPProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}},
		"clientInfo":         map[string]string{"name": "agent-sessions", "version": PinnedVersion},
	}, &response)
	if err != nil {
		return err
	}
	if response.ProtocolVersion != ACPProtocolVersion {
		return fmt.Errorf("%w: DSH ACP protocol version %d", productruntime.ErrIncompatible, response.ProtocolVersion)
	}
	return nil
}

func (client *ACPClient) Request(ctx context.Context, method string, params any, output any) error {
	future, err := client.startRequest(ctx, method, params, nil)
	if err != nil {
		return err
	}
	result, err := waitRPC(ctx, future)
	if err != nil {
		return mapACPError(method, err)
	}
	if output == nil {
		return nil
	}
	if len(result) == 0 || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		return fmt.Errorf("%w: DSH ACP %s omitted its required result", productruntime.ErrProtocol, method)
	}
	if err := json.Unmarshal(result, output); err != nil {
		return fmt.Errorf("%w: decode DSH ACP %s result: %v", productruntime.ErrProtocol, method, err)
	}
	return nil
}

func (client *ACPClient) Notify(ctx context.Context, method string, params any) error {
	if strings.TrimSpace(method) == "" {
		return fmt.Errorf("%w: DSH ACP notification method is empty", productruntime.ErrProtocol)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("%w: encode DSH ACP notification: %v", productruntime.ErrProtocol, err)
	}
	return client.write(ctx, body)
}

func (client *ACPClient) startRequest(ctx context.Context, method string, params any, beforeWrite func(rpcFuture)) (rpcFuture, error) {
	if strings.TrimSpace(method) == "" {
		return rpcFuture{}, fmt.Errorf("%w: DSH ACP request method is empty", productruntime.ErrProtocol)
	}
	client.mu.Lock()
	if client.closed != nil {
		err := client.closed
		client.mu.Unlock()
		return rpcFuture{}, err
	}
	client.nextID++
	id := fmt.Sprintf("as-%d", client.nextID)
	response := make(chan rpcResult, 1)
	client.pending[id] = response
	future := rpcFuture{id: id, method: method, done: response}
	if beforeWrite != nil {
		beforeWrite(future)
	}
	client.mu.Unlock()

	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err == nil && len(body) > maxACPFrameBytes {
		err = fmt.Errorf("%w: DSH ACP request exceeds fixed frame bound", productruntime.ErrProtocol)
	}
	if err != nil {
		client.dropPending(id)
		return rpcFuture{}, err
	}
	if err := client.write(ctx, body); err != nil {
		client.dropPending(id)
		return rpcFuture{}, &possibleACPWriteError{err: err}
	}
	return future, nil
}

func (client *ACPClient) write(ctx context.Context, body []byte) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if err := client.process.WriteFrame(ctx, body); err != nil {
		return fmt.Errorf("%w: write DSH ACP frame: %v", productruntime.ErrProtocol, err)
	}
	return nil
}

func (client *ACPClient) dropPending(id string) {
	client.mu.Lock()
	delete(client.pending, id)
	client.mu.Unlock()
}

func (client *ACPClient) readLoop() {
	for {
		body, err := client.process.ReadFrame(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = fmt.Errorf("%w: DSH ACP process closed", productruntime.ErrUnavailable)
			} else {
				err = fmt.Errorf("%w: read DSH ACP frame: %v", productruntime.ErrProtocol, err)
			}
			client.fail(err)
			return
		}
		if len(body) == 0 || len(body) > maxACPFrameBytes || !json.Valid(body) {
			client.fail(fmt.Errorf("%w: invalid DSH ACP frame", productruntime.ErrProtocol))
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var envelope struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := decoder.Decode(&envelope); err != nil || envelope.JSONRPC != "2.0" {
			client.fail(fmt.Errorf("%w: malformed DSH ACP envelope", productruntime.ErrProtocol))
			return
		}
		hasResult, hasError := len(envelope.Result) != 0, len(envelope.Error) != 0
		if envelope.Method != "" && (hasResult || hasError) {
			client.fail(fmt.Errorf("%w: DSH ACP method envelope mixed response fields", productruntime.ErrProtocol))
			return
		}
		if envelope.Method != "" && len(envelope.ID) == 0 {
			if client.notify != nil {
				client.notify(acpNotification{Method: envelope.Method, Params: append(json.RawMessage(nil), envelope.Params...)})
			}
			continue
		}
		if envelope.Method != "" {
			if !validACPRequestID(envelope.ID) {
				client.fail(fmt.Errorf("%w: DSH ACP server request id is invalid", productruntime.ErrProtocol))
				return
			}
			result, failure := any(nil), &rpcError{Code: -32601, Message: "unsupported ACP client method"}
			if client.request != nil {
				result, failure = client.request(acpServerRequest{Method: envelope.Method, Params: append(json.RawMessage(nil), envelope.Params...)})
			}
			if err := client.reply(envelope.ID, result, failure); err != nil {
				client.fail(err)
				return
			}
			continue
		}
		if hasResult == hasError {
			client.fail(fmt.Errorf("%w: DSH ACP response must contain exactly one of result or error", productruntime.ErrProtocol))
			return
		}
		var responseError *rpcError
		if hasError && (bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) || json.Unmarshal(envelope.Error, &responseError) != nil || responseError == nil) {
			client.fail(fmt.Errorf("%w: DSH ACP response error is malformed", productruntime.ErrProtocol))
			return
		}
		var id string
		if err := json.Unmarshal(envelope.ID, &id); err != nil || id == "" {
			client.fail(fmt.Errorf("%w: DSH ACP response id is invalid", productruntime.ErrProtocol))
			return
		}
		client.mu.Lock()
		pending := client.pending[id]
		delete(client.pending, id)
		client.mu.Unlock()
		if pending == nil {
			client.fail(fmt.Errorf("%w: DSH ACP response id is unknown", productruntime.ErrProtocol))
			return
		}
		if hasError {
			pending <- rpcResult{err: responseError}
		} else {
			pending <- rpcResult{result: append(json.RawMessage(nil), envelope.Result...)}
		}
	}
}

func validACPRequestID(id json.RawMessage) bool {
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return false
	}
	var text string
	if json.Unmarshal(id, &text) == nil {
		return text != "" && len(text) <= 256
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil || number.String() == "" {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func (client *ACPClient) reply(id json.RawMessage, result any, failure *rpcError) error {
	envelope := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(append([]byte(nil), id...))}
	if failure != nil {
		envelope["error"] = failure
	} else {
		envelope["result"] = result
	}
	body, err := json.Marshal(envelope)
	if err != nil || len(body) > maxACPFrameBytes {
		return fmt.Errorf("%w: encode bounded DSH ACP server response", productruntime.ErrProtocol)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.write(ctx, body)
}

func (client *ACPClient) fail(err error) {
	client.mu.Lock()
	if client.closed != nil {
		client.mu.Unlock()
		return
	}
	client.closed = err
	pending := client.pending
	client.pending = make(map[string]chan rpcResult)
	client.mu.Unlock()
	for _, response := range pending {
		response <- rpcResult{err: err}
	}
}

func waitRPC(ctx context.Context, future rpcFuture) (json.RawMessage, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", productruntime.ErrTimedOut, ctx.Err())
	case response := <-future.done:
		return response.result, response.err
	}
}

func mapACPError(method string, err error) error {
	var native *rpcError
	if errors.As(err, &native) {
		if method == "session/prompt" && native.Code == -32602 {
			return fmt.Errorf("%w: DSH ACP prompt is already in flight", productruntime.ErrUnsupportedSteer)
		}
		if native.Code == -32603 && strings.Contains(strings.ToLower(native.Message), "auth") {
			return fmt.Errorf("%w: DSH model authentication is unavailable", productruntime.ErrUnauthorized)
		}
		return fmt.Errorf("%w: %v", productruntime.ErrNativeRejected, native)
	}
	return err
}
