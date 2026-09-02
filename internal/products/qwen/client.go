package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type rpcProcess interface {
	ReadFrame(context.Context) ([]byte, error)
	WriteFrame(context.Context, []byte) error
	Cleanup(context.Context) error
}

type rpcResult struct {
	value map[string]any
	err   error
}

type rpcFuture struct {
	id   int64
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	got  rpcResult
}

func newRPCFuture(id int64) *rpcFuture {
	return &rpcFuture{id: id, done: make(chan struct{})}
}

func (future *rpcFuture) resolve(result rpcResult) {
	future.once.Do(func() {
		future.mu.Lock()
		future.got = result
		future.mu.Unlock()
		close(future.done)
	})
}

func (future *rpcFuture) wait(ctx context.Context) (map[string]any, error) {
	if ctx == nil {
		return nil, errors.New("Qwen ACP wait requires context")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-future.done:
		future.mu.Lock()
		defer future.mu.Unlock()
		return future.got.value, future.got.err
	}
}

type rpcError struct {
	Code    int
	Message string
	Data    any
}

func (err *rpcError) Error() string {
	if err.Code == 0 {
		return err.Message
	}
	return fmt.Sprintf("Qwen ACP error %d: %s", err.Code, err.Message)
}

type rpcClient struct {
	process rpcProcess

	writeMu sync.Mutex
	stateMu sync.Mutex
	nextID  int64
	pending map[int64]*rpcFuture
	failed  error

	notifyMu sync.RWMutex
	notify   func(map[string]any)
}

func newRPCClient(process rpcProcess) (*rpcClient, error) {
	if process == nil {
		return nil, errors.New("Qwen ACP process is nil")
	}
	client := &rpcClient{process: process, pending: make(map[int64]*rpcFuture)}
	go client.readLoop()
	return client, nil
}

func (client *rpcClient) setNotificationHandler(handler func(map[string]any)) {
	client.notifyMu.Lock()
	client.notify = handler
	client.notifyMu.Unlock()
}

func (client *rpcClient) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	future, err := client.start(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return future.wait(ctx)
}

func (client *rpcClient) start(ctx context.Context, method string, params map[string]any) (*rpcFuture, error) {
	if ctx == nil {
		return nil, errors.New("Qwen ACP request requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client.stateMu.Lock()
	if client.failed != nil {
		err := client.failed
		client.stateMu.Unlock()
		return nil, err
	}
	client.nextID++
	future := newRPCFuture(client.nextID)
	client.pending[future.id] = future
	client.stateMu.Unlock()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": future.id, "method": method, "params": params})
	if err == nil {
		err = client.writeFrame(ctx, body)
	}
	if err != nil {
		client.removePending(future.id)
		return nil, err
	}
	return future, nil
}

func (client *rpcClient) readLoop() {
	for {
		body, err := client.process.ReadFrame(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.EOF
			}
			client.fail(err)
			return
		}
		var message map[string]any
		if json.Unmarshal(body, &message) != nil {
			client.fail(errors.New("malformed Qwen ACP frame"))
			return
		}
		method, _ := message["method"].(string)
		id, hasID := rpcID(message["id"])
		switch {
		case hasID && method == "session/request_permission":
			go client.answerPermission(id, mapValue(message["params"]))
		case hasID && method != "":
			go client.writeResponse(id, nil, fmt.Errorf("unsupported Qwen ACP client request %q", method))
		case hasID:
			client.deliver(id, message)
		case method != "":
			client.notifyMu.RLock()
			handler := client.notify
			client.notifyMu.RUnlock()
			if handler != nil {
				handler(message)
			}
		default:
			client.fail(errors.New("malformed Qwen ACP frame"))
			return
		}
	}
}

func (client *rpcClient) answerPermission(id int64, params map[string]any) {
	options, _ := params["options"].([]any)
	outcome := map[string]any{"outcome": "cancelled"}
	for _, raw := range options {
		option := mapValue(raw)
		kind, _ := option["kind"].(string)
		optionID, _ := option["optionId"].(string)
		if kind == "allow_once" && optionID != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": optionID}
			break
		}
	}
	client.writeResponse(id, map[string]any{"outcome": outcome}, nil)
}

func (client *rpcClient) writeResponse(id int64, result map[string]any, responseErr error) {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if responseErr != nil {
		response["error"] = map[string]any{"code": -32601, "message": responseErr.Error()}
	} else {
		response["result"] = result
	}
	body, err := json.Marshal(response)
	if err == nil {
		err = client.writeFrame(context.Background(), body)
	}
	if err != nil {
		client.fail(err)
	}
}

func (client *rpcClient) writeFrame(ctx context.Context, body []byte) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	return client.process.WriteFrame(ctx, body)
}

func (client *rpcClient) deliver(id int64, message map[string]any) {
	client.stateMu.Lock()
	future := client.pending[id]
	delete(client.pending, id)
	client.stateMu.Unlock()
	if future == nil {
		return
	}
	if raw := mapValue(message["error"]); len(raw) != 0 {
		code, _ := raw["code"].(float64)
		text, _ := raw["message"].(string)
		future.resolve(rpcResult{err: &rpcError{Code: int(code), Message: text, Data: raw["data"]}})
		return
	}
	future.resolve(rpcResult{value: mapValue(message["result"])})
}

func (client *rpcClient) removePending(id int64) {
	client.stateMu.Lock()
	delete(client.pending, id)
	client.stateMu.Unlock()
}

func (client *rpcClient) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	client.stateMu.Lock()
	if client.failed != nil {
		client.stateMu.Unlock()
		return
	}
	client.failed = err
	pending := client.pending
	client.pending = make(map[int64]*rpcFuture)
	client.stateMu.Unlock()
	for _, future := range pending {
		future.resolve(rpcResult{err: err})
	}
}

func rpcID(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}
