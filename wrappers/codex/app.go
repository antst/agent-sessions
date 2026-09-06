package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type appError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *appError) Error() string { return e.Message }

type appReply struct {
	result json.RawMessage
	err    error
}

type appTransport interface {
	Read(any) error
	Write(any) error
	Close() error
}

type streamTransport struct {
	input   io.WriteCloser
	output  io.ReadCloser
	decoder *json.Decoder
}

func (s *streamTransport) Read(value any) error  { return s.decoder.Decode(value) }
func (s *streamTransport) Write(value any) error { return json.NewEncoder(s.input).Encode(value) }
func (s *streamTransport) Close() error          { return s.input.Close() }

type appFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *appError       `json:"error"`
}

type appClient struct {
	transport   appTransport
	mu, writeMu sync.Mutex
	next        int64
	pending     map[int64]chan appReply
	failed      error
	notify      func(string, json.RawMessage)
	onFailure   func(error)
}

func newAppClient(input io.WriteCloser, output io.ReadCloser, notify func(string, json.RawMessage), failure func(error)) *appClient {
	return newTransportClient(&streamTransport{input: input, output: output, decoder: json.NewDecoder(output)}, notify, failure)
}

func newTransportClient(transport appTransport, notify func(string, json.RawMessage), failure func(error)) *appClient {
	c := &appClient{transport: transport, pending: map[int64]chan appReply{}, notify: notify, onFailure: failure}
	go c.read()
	return c
}

func (c *appClient) initialize(ctx context.Context, title string) error {
	err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": Product, "title": title, "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &struct{}{})
	if err == nil {
		err = c.notifyMethod("initialized", map[string]any{})
	}
	return err
}

func (c *appClient) call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.failed != nil {
		err := c.failed
		c.mu.Unlock()
		return err
	}
	c.next++
	id, reply := c.next, make(chan appReply, 1)
	c.pending[id] = reply
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.remove(id)
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case response := <-reply:
		if response.err != nil {
			return response.err
		}
		if result == nil || len(response.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.result, result); err != nil {
			return fmt.Errorf("decode App Server %s result: %w", method, err)
		}
		return nil
	}
}

func (c *appClient) notifyMethod(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *appClient) write(value any) error {
	c.writeMu.Lock()
	err := c.transport.Write(value)
	c.writeMu.Unlock()
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *appClient) read() {
	if stream, ok := c.transport.(*streamTransport); ok {
		defer stream.output.Close()
	}
	for {
		var frame appFrame
		if err := c.transport.Read(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("Codex App Server exited")
			} else {
				err = fmt.Errorf("malformed Codex App Server frame: %w", err)
			}
			c.fail(err)
			return
		}
		if frame.Method != "" {
			if len(frame.ID) != 0 {
				go c.write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": appError{Code: -32601, Message: "headless Codex request is unsupported"}})
			} else if c.notify != nil {
				c.notify(frame.Method, frame.Params)
			}
			continue
		}
		var id int64
		if json.Unmarshal(frame.ID, &id) != nil || id < 1 {
			c.fail(errors.New("Codex App Server returned an invalid response id"))
			return
		}
		if (frame.Error == nil) == (len(frame.Result) == 0) {
			c.fail(errors.New("Codex App Server response must contain exactly one of result or error"))
			return
		}
		c.mu.Lock()
		reply := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if reply == nil {
			c.fail(errors.New("Codex App Server returned an unknown response id"))
			return
		}
		if frame.Error != nil {
			reply <- appReply{err: frame.Error}
		} else {
			reply <- appReply{result: frame.Result}
		}
	}
}

func (c *appClient) remove(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *appClient) fail(err error) {
	c.mu.Lock()
	if c.failed != nil {
		c.mu.Unlock()
		return
	}
	c.failed = err
	pending := c.pending
	c.pending = map[int64]chan appReply{}
	c.mu.Unlock()
	for _, reply := range pending {
		reply <- appReply{err: err}
	}
	_ = c.transport.Close()
	if c.onFailure != nil {
		c.onFailure(err)
	}
}

func (c *appClient) close() error {
	return c.transport.Close()
}
