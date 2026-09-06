package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const maxACPFrame = 1 << 20

type acpFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

type acpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpError) Error() string { return fmt.Sprintf("Grok ACP error %d: %s", e.Code, e.Message) }

type interjectionNotice struct{ SessionID, InterjectionID string }

type acpClient struct {
	input       io.WriteCloser
	requests    sync.Mutex
	writes      sync.Mutex
	next        int64
	err         error
	responses   chan acpFrame
	interjected chan interjectionNotice
	done        chan struct{}
	notify      func(acpFrame)
}

func newACPClient(input io.WriteCloser, output io.Reader, notify func(acpFrame)) *acpClient {
	c := &acpClient{input: input, responses: make(chan acpFrame, 16), interjected: make(chan interjectionNotice, 16), done: make(chan struct{}), notify: notify}
	go c.read(output)
	return c
}

func (c *acpClient) read(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), maxACPFrame)
	for scanner.Scan() {
		var frame acpFrame
		body := scanner.Bytes()
		if !json.Valid(body) || json.Unmarshal(body, &frame) != nil || frame.JSONRPC != "2.0" {
			c.finish(errors.New("malformed Grok ACP frame"))
			return
		}
		if frame.ID != nil {
			c.responses <- frame
			continue
		}
		if frame.Method == "_x.ai/session/interjection" {
			var notice struct {
				SessionID      string `json:"sessionId"`
				InterjectionID string `json:"interjectionId"`
			}
			if json.Unmarshal(frame.Params, &notice) == nil {
				c.interjected <- interjectionNotice(notice)
			}
		}
		if c.notify != nil {
			c.notify(frame)
		}
	}
	if err := scanner.Err(); err != nil {
		c.finish(err)
	} else {
		c.finish(io.EOF)
	}
}

func (c *acpClient) finish(err error) {
	c.err = err
	close(c.done)
}

func (c *acpClient) request(ctx context.Context, method string, params any, result any) error {
	return c.requestStarted(ctx, method, params, result, nil)
}

func (c *acpClient) requestStarted(ctx context.Context, method string, params any, result any, started chan<- error) error {
	c.requests.Lock()
	defer c.requests.Unlock()
	c.next++
	id := c.next
	err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if started != nil {
		started <- err
	}
	if err != nil {
		return fmt.Errorf("write Grok ACP %s: %w", method, err)
	}
	for {
		select {
		case frame := <-c.responses:
			if frame.ID == nil || *frame.ID != id {
				continue
			}
			if frame.Error != nil {
				return frame.Error
			}
			if result != nil && json.Unmarshal(frame.Result, result) != nil {
				return fmt.Errorf("decode Grok ACP %s response", method)
			}
			return nil
		case <-c.done:
			return fmt.Errorf("read Grok ACP %s: %w", method, c.err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *acpClient) send(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writes.Lock()
	defer c.writes.Unlock()
	_, err = c.input.Write(append(body, '\n'))
	return err
}

func (c *acpClient) cancel(sessionID string) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": sessionID}})
}

func (c *acpClient) interject(ctx context.Context, sessionID, messageID, text string) error {
	var reply struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := c.request(ctx, "_x.ai/interject", map[string]string{"sessionId": sessionID, "text": text, "interjectionId": messageID}, &reply); err != nil {
		return err
	}
	if reply.Result.Status != "queued" {
		return errors.New("Grok interjection was not queued")
	}
	for {
		select {
		case notice := <-c.interjected:
			if notice.SessionID == sessionID && notice.InterjectionID == messageID {
				return nil
			}
		case <-c.done:
			return errors.New("Grok leader closed before interjection acknowledgement")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *acpClient) close() { _ = c.input.Close() }
