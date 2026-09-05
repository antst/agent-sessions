// Package conn owns the socket reader and writer used by an Agentbus session.
package conn

import (
	"bufio"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

const OutboxSize = 256

type Frame struct {
	Value protocol.Frame
	Err   error
}

type Closed struct {
	Connection *Conn
	Cause      error
}

type Conn struct {
	fd        net.Conn
	inbox     chan any
	outbox    chan []byte
	done      chan struct{}
	closed    atomic.Bool
	finishing bool
}

func Start(fd net.Conn, group *sync.WaitGroup) *Conn {
	c := &Conn{fd: fd, inbox: make(chan any, OutboxSize), outbox: make(chan []byte, OutboxSize), done: make(chan struct{})}
	group.Add(2)
	go func() { defer group.Done(); c.read() }()
	go func() { defer group.Done(); c.write() }()
	return c
}

func (c *Conn) Done() <-chan struct{} { return c.done }
func (c *Conn) Inbox() <-chan any     { return c.inbox }

func (c *Conn) Post(event any) bool {
	if c.closed.Load() {
		return false
	}
	select {
	case c.inbox <- event:
		return true
	default:
		return false
	}
}

// Send is called only by the connection's owner loop.
func (c *Conn) Send(frame []byte) bool {
	if c.closed.Load() || c.finishing {
		return false
	}
	select {
	case c.outbox <- frame:
		return true
	default:
		c.Close()
		return false
	}
}

// Finish queues the last frame, bounds its write, then closes the outbox.
// It is called only by the connection's owner loop.
func (c *Conn) Finish(frame []byte, bound time.Duration) bool {
	if c.finishing || c.closed.Load() {
		return false
	}
	c.finishing = true
	_ = c.fd.SetWriteDeadline(time.Now().Add(bound))
	select {
	case c.outbox <- frame:
		close(c.outbox)
		return true
	default:
		c.Close()
		return false
	}
}

func (c *Conn) Close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
		_ = c.fd.Close()
	}
}

func (c *Conn) read() {
	reader := bufio.NewReaderSize(c.fd, protocol.MaxFrameBytes)
	for {
		body, err := reader.ReadSlice('\n')
		if err != nil {
			c.Close()
			c.inbox <- Closed{Connection: c, Cause: err}
			return
		}
		value, decodeErr := protocol.DecodeFrame(body[:len(body)-1])
		c.inbox <- Frame{Value: value, Err: decodeErr}
	}
}

func (c *Conn) write() {
	for {
		select {
		case <-c.done:
			return
		case frame, ok := <-c.outbox:
			if !ok {
				c.Close()
				return
			}
			if err := writeFull(c.fd, frame); err != nil {
				c.Close()
				return
			}
		}
	}
}

func writeFull(writer io.Writer, body []byte) error {
	for len(body) != 0 {
		written, err := writer.Write(body)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}
