package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type acpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpError) Error() string { return fmt.Sprintf("Qwen ACP error %d: %s", e.Code, e.Message) }

type acpFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *acpError       `json:"error"`
}

type acpReply struct {
	result json.RawMessage
	err    error
}

type acpClient struct {
	input       io.WriteCloser
	output      io.ReadCloser
	decoder     *json.Decoder
	writeMu, mu sync.Mutex
	next        int64
	pending     map[int64]chan acpReply
	failed      error
	done        chan struct{}
	notify      func(string, json.RawMessage)
	drain       func() []string
}

func newACPClient(input io.WriteCloser, output io.ReadCloser, notify func(string, json.RawMessage), drain func() []string) *acpClient {
	c := &acpClient{input: input, output: output, decoder: json.NewDecoder(output), pending: map[int64]chan acpReply{}, done: make(chan struct{}), notify: notify, drain: drain}
	go c.read()
	return c
}

func (c *acpClient) call(ctx context.Context, method string, params, result any) error {
	reply, err := c.start(ctx, method, params)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case got := <-reply:
		if got.err != nil || result == nil {
			return got.err
		}
		if err = json.Unmarshal(got.result, result); err != nil {
			return fmt.Errorf("decode Qwen ACP %s result: %w", method, err)
		}
		return nil
	}
}

func (c *acpClient) start(ctx context.Context, method string, params any) (<-chan acpReply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.failed != nil {
		err := c.failed
		c.mu.Unlock()
		return nil, err
	}
	c.next++
	id, reply := c.next, make(chan acpReply, 1)
	c.pending[id] = reply
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	return reply, nil
}

func (c *acpClient) write(value any) error {
	c.writeMu.Lock()
	err := json.NewEncoder(c.input).Encode(value)
	c.writeMu.Unlock()
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *acpClient) read() {
	defer close(c.done)
	defer c.output.Close()
	for {
		var frame acpFrame
		if err := c.decoder.Decode(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("Qwen ACP exited")
			} else {
				err = fmt.Errorf("malformed Qwen ACP frame: %w", err)
			}
			c.fail(err)
			return
		}
		if frame.Method != "" {
			if len(frame.ID) == 0 {
				if c.notify != nil {
					c.notify(frame.Method, frame.Params)
				}
			} else {
				go c.answer(frame)
			}
			continue
		}
		var id int64
		if json.Unmarshal(frame.ID, &id) != nil || id < 1 || (frame.Error == nil) == (len(frame.Result) == 0) {
			c.fail(errors.New("Qwen ACP returned an invalid response"))
			return
		}
		c.mu.Lock()
		reply := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if reply == nil {
			c.fail(errors.New("Qwen ACP returned an unknown response id"))
			return
		}
		if frame.Error != nil {
			reply <- acpReply{err: frame.Error}
		} else {
			reply <- acpReply{result: frame.Result}
		}
	}
}

func (c *acpClient) answer(frame acpFrame) {
	var result any
	var failure *acpError
	switch frame.Method {
	case "session/request_permission":
		result = map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
	case "craft/drainMidTurnQueue":
		messages := c.drain()
		result = map[string]any{"messages": messages, "hasQueuedPrompt": len(messages) > 0}
	default:
		failure = &acpError{Code: -32601, Message: "unsupported Qwen ACP client request " + frame.Method}
	}
	response := map[string]any{"jsonrpc": "2.0", "id": frame.ID}
	if failure != nil {
		response["error"] = failure
	} else {
		response["result"] = result
	}
	_ = c.write(response)
}

func (c *acpClient) fail(err error) {
	c.mu.Lock()
	if c.failed != nil {
		c.mu.Unlock()
		return
	}
	pending := c.pending
	c.failed, c.pending = err, map[int64]chan acpReply{}
	c.mu.Unlock()
	for _, reply := range pending {
		reply <- acpReply{err: err}
	}
}

func (c *acpClient) close() error { return c.input.Close() }
