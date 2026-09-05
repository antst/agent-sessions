package rpc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

var ErrClosed = errors.New("agentbus connection closed")

type Request struct {
	ID     int64
	Method string
	Params any
}

type pending struct {
	method string
	result any
	done   chan error
	seen   func() error
}

type Conn struct {
	fd      net.Conn
	reader  *bufio.Reader
	client  bool
	handler func(context.Context, *Request)
	ctx     context.Context
	cancel  context.CancelFunc

	writeMu, stateMu sync.Mutex
	closed           bool
	next, last       int64
	pending          map[int64]pending
}

func New(fd net.Conn, client bool, handler func(context.Context, *Request)) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{fd: fd, reader: bufio.NewReaderSize(fd, protocol.MaxFrameBytes), client: client, handler: handler, ctx: ctx, cancel: cancel, pending: map[int64]pending{}}
	go c.read()
	return c
}

func (c *Conn) Done() <-chan struct{}    { return c.ctx.Done() }
func (c *Conn) Context() context.Context { return c.ctx }

func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, nil)
}

// CallObserved runs seen after a valid result is decoded and before the reader
// accepts the next frame.
func (c *Conn) CallObserved(ctx context.Context, method string, params, result any, seen func() error) error {
	return c.call(ctx, method, params, result, seen)
}

func (c *Conn) call(ctx context.Context, method string, params, result any, seen func() error) error {
	id, p := c.begin(method, params, result, seen)
	select {
	case err := <-p.done:
		return err
	case <-ctx.Done():
		if c.drop(id, p) {
			return ctx.Err()
		}
		return <-p.done
	}
}

// Begin sends one call and returns its eventual response without tying it to a
// caller context. It lets a connection owner serialize writes without waiting
// for earlier responses.
func (c *Conn) Begin(method string, params, result any, seen func() error) <-chan error {
	_, p := c.begin(method, params, result, seen)
	return p.done
}

func (c *Conn) begin(method string, params, result any, seen func() error) (int64, pending) {
	raw, err := protocol.EncodeParams(method, params)
	p := pending{method: method, result: result, done: make(chan error, 1), seen: seen}
	if err != nil {
		p.done <- err
		return 0, p
	}
	c.writeMu.Lock()
	c.stateMu.Lock()
	var id int64
	if c.closed {
		err = ErrClosed
	} else if c.next == protocol.MaxRequestID {
		err = errors.New("request id space exhausted")
	} else {
		c.next++
		id = c.next
		c.pending[id] = p
	}
	c.stateMu.Unlock()
	var body []byte
	if err == nil {
		body, err = protocol.EncodeRequest(id, method, raw)
	}
	if err == nil {
		err = c.send(body)
	}
	c.writeMu.Unlock()
	if err != nil {
		if id == 0 || c.drop(id, p) {
			p.done <- err
		}
	}
	return id, p
}

func (c *Conn) Result(request *Request, value any) error {
	body, err := protocol.ResultBytes(request.ID, request.Method, value)
	return c.finish(request, body, err)
}

func (c *Conn) Error(request *Request, code int, data any) error {
	body, err := protocol.ErrorBytes(request.ID, code, data)
	return c.finish(request, body, err)
}

func (c *Conn) finish(request *Request, body []byte, err error) error {
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = c.send(body)
	c.writeMu.Unlock()
	return err
}

func (c *Conn) send(body []byte) error {
	c.stateMu.Lock()
	closed := c.closed
	c.stateMu.Unlock()
	if closed {
		return ErrClosed
	}
	n, err := c.fd.Write(body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		c.close(err)
	}
	return err
}

func (c *Conn) read() {
	for {
		body, err := c.reader.ReadSlice('\n')
		if err != nil {
			c.close(err)
			return
		}
		frame, err := protocol.DecodeFrame(body[:len(body)-1])
		if err != nil {
			if frame.ID > 0 && frame.Request {
				c.reject(frame, protocol.InvalidFrame)
			} else {
				c.close(err)
			}
			return
		}
		if frame.Request {
			c.receiveRequest(frame)
		} else {
			c.receiveResponse(frame)
		}
	}
}

func (c *Conn) receiveRequest(frame protocol.Frame) {
	c.stateMu.Lock()
	ordered := frame.ID > c.last
	if ordered {
		c.last = frame.ID
	}
	c.stateMu.Unlock()
	if !ordered {
		c.reject(frame, protocol.InvalidFrame)
		return
	}
	params, err := protocol.DecodeParams(frame.Method, frame.Params)
	request := &Request{ID: frame.ID, Method: frame.Method, Params: params}
	if err != nil || !protocol.Allows(frame.Method, !c.client) {
		code := protocol.InvalidFrame
		if frame.Method == "session.hello" {
			code = protocol.InvalidHello
		}
		c.reject(frame, code)
		return
	}
	c.handler(c.ctx, request)
}

func (c *Conn) reject(frame protocol.Frame, code int) {
	_ = c.Error(&Request{ID: frame.ID, Method: frame.Method}, code, nil)
	c.close(errors.New("invalid frame"))
}

func (c *Conn) receiveResponse(frame protocol.Frame) {
	c.stateMu.Lock()
	p, ok := c.pending[frame.ID]
	if ok {
		delete(c.pending, frame.ID)
	}
	c.stateMu.Unlock()
	if !ok {
		c.close(errors.New("unmatched response"))
		return
	}
	err := error(frame.Error)
	if frame.Error == nil {
		if err = protocol.UnmarshalResult(p.method, frame.Result, p.result); err != nil {
			c.close(err)
		} else if p.seen != nil {
			err = p.seen()
			if err != nil {
				c.close(err)
			}
		}
	}
	p.done <- err
}

func (c *Conn) drop(id int64, want pending) bool {
	c.stateMu.Lock()
	got := c.pending[id]
	ok := got.done == want.done
	if ok {
		delete(c.pending, id)
	}
	c.stateMu.Unlock()
	return ok
}

func (c *Conn) Close() error { c.close(ErrClosed); return nil }

func (c *Conn) close(cause error) {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return
	}
	c.closed = true
	calls := c.pending
	c.pending = map[int64]pending{}
	c.stateMu.Unlock()
	c.cancel()
	_ = c.fd.Close()
	for _, call := range calls {
		call.done <- cause
	}
}
