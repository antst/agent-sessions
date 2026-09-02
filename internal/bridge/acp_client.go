package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type qwenACPClient struct {
	stdin io.WriteCloser

	writeMu sync.Mutex
	stateMu sync.Mutex
	nextID  int64
	pending map[int64]chan qwenACPResponse
	readErr error
	done    chan struct{}

	notifyMu sync.RWMutex
	notify   func(map[string]any)
}

type qwenACPResponse struct {
	result map[string]any
	err    error
}

type qwenRPCError struct {
	Code    int
	Message string
	Data    any
}

func (e *qwenRPCError) Error() string {
	if e.Code == 0 {
		return "Qwen ACP: " + e.Message
	}
	return fmt.Sprintf("Qwen ACP error %d: %s", e.Code, e.Message)
}

func newQwenACPClient(stdin io.WriteCloser, stdout io.ReadCloser) *qwenACPClient {
	client := &qwenACPClient{stdin: stdin, pending: map[int64]chan qwenACPResponse{}, done: make(chan struct{})}
	go client.readLoop(stdout)
	return client
}

func (c *qwenACPClient) setNotificationHandler(handler func(map[string]any)) {
	c.notifyMu.Lock()
	c.notify = handler
	c.notifyMu.Unlock()
}

func (c *qwenACPClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.stateMu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.stateMu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	response := make(chan qwenACPResponse, 1)
	c.pending[id] = response
	c.stateMu.Unlock()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err == nil {
		err = c.writeFrame(body)
	}
	if err != nil {
		c.removePending(id)
		return nil, err
	}
	select {
	case received := <-response:
		return received.result, received.err
	case <-c.done:
		c.removePending(id)
		return nil, c.readError()
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	}
}

func (c *qwenACPClient) notifyCancel(params map[string]any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": params})
	if err != nil {
		return err
	}
	return c.writeFrame(body)
}

func (c *qwenACPClient) close() error { return c.stdin.Close() }

func (c *qwenACPClient) readLoop(stdout io.ReadCloser) {
	defer func() { _ = stdout.Close() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 64*1024*1024)
	for scanner.Scan() {
		var message map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			c.fail(err)
			return
		}
		method := stringValue(message["method"])
		id, hasID := qwenRPCID(message["id"])
		switch {
		case hasID && method == "session/request_permission":
			go c.answerPermission(id, mapValue(message["params"]))
		case hasID && method != "":
			go c.answerUnsupported(id, method)
		case hasID:
			c.deliverResponse(id, message)
		case method != "":
			c.notifyMu.RLock()
			handler := c.notify
			c.notifyMu.RUnlock()
			if handler != nil {
				handler(message)
			}
		default:
			c.fail(errors.New("malformed Qwen ACP frame"))
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(err)
}

func (c *qwenACPClient) answerPermission(id int64, params map[string]any) {
	options, _ := params["options"].([]any)
	outcome := map[string]any{"outcome": "cancelled"}
	for _, raw := range options {
		option := mapValue(raw)
		if stringValue(option["kind"]) == "allow_once" && stringValue(option["optionId"]) != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": stringValue(option["optionId"])}
			break
		}
	}
	c.writeResponse(id, map[string]any{"outcome": outcome}, nil)
}

func (c *qwenACPClient) answerUnsupported(id int64, method string) {
	c.writeResponse(id, nil, fmt.Errorf("unsupported Qwen ACP client request %q", method))
}

func (c *qwenACPClient) writeResponse(id int64, result map[string]any, responseErr error) {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if responseErr != nil {
		response["error"] = map[string]any{"code": -32601, "message": responseErr.Error()}
	} else {
		response["result"] = result
	}
	body, err := json.Marshal(response)
	if err == nil {
		err = c.writeFrame(body)
	}
	if err != nil {
		c.fail(err)
	}
}

func (c *qwenACPClient) deliverResponse(id int64, message map[string]any) {
	c.stateMu.Lock()
	response := c.pending[id]
	delete(c.pending, id)
	c.stateMu.Unlock()
	if response == nil {
		return
	}
	if raw := mapValue(message["error"]); len(raw) != 0 {
		response <- qwenACPResponse{err: &qwenRPCError{Code: intValue(raw["code"]), Message: stringValue(raw["message"]), Data: raw["data"]}}
		return
	}
	response <- qwenACPResponse{result: mapValue(message["result"])}
}

func (c *qwenACPClient) writeFrame(body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.stdin.Write(append(body, '\n'))
	return err
}

func (c *qwenACPClient) removePending(id int64) {
	c.stateMu.Lock()
	delete(c.pending, id)
	c.stateMu.Unlock()
}

func (c *qwenACPClient) fail(err error) {
	c.stateMu.Lock()
	if c.readErr != nil {
		c.stateMu.Unlock()
		return
	}
	c.readErr = err
	pending := c.pending
	c.pending = map[int64]chan qwenACPResponse{}
	close(c.done)
	c.stateMu.Unlock()
	for _, response := range pending {
		response <- qwenACPResponse{err: err}
	}
}

func (c *qwenACPClient) readError() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.readErr == nil {
		return io.EOF
	}
	return c.readErr
}

func qwenRPCID(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case interface{ Int64() (int64, error) }:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func qwenACPMode(result map[string]any) string {
	return stringValue(mapValue(result["modes"])["currentModeId"])
}
