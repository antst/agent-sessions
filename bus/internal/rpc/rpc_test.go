package rpc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

func TestCallRoundTripAndIncreasingIDs(t *testing.T) {
	client, peer := net.Pipe()
	c := New(client, true, nil)
	defer c.Close()
	go func() {
		for id := int64(1); id <= 2; id++ {
			frame := readFrame(t, peer)
			if frame.ID != id {
				t.Errorf("id = %d, want %d", frame.ID, id)
			}
			body, _ := protocol.ResultBytes(id, "session.list", protocol.SessionListResult{Sessions: []protocol.SessionSummary{}})
			_, _ = peer.Write(body)
		}
	}()
	for range 2 {
		var result protocol.SessionListResult
		if err := c.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloseBeforeWriteEmitsNothing(t *testing.T) {
	local, peer := net.Pipe()
	paused := &pauseWriteConn{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
	c := New(paused, true, nil)
	returned := asyncCall(c, context.Background())
	<-paused.entered
	_ = c.Close()
	close(paused.release)
	if err := <-returned; err == nil {
		t.Fatal("call succeeded")
	}
	_ = peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if body, _ := bufio.NewReader(peer).ReadBytes('\n'); len(body) != 0 {
		t.Fatalf("wrote %q", body)
	}
}

func TestCloseUnblocksWriter(t *testing.T) {
	local, peer := net.Pipe()
	entered := make(chan struct{})
	c := New(&signalWriteConn{Conn: local, entered: entered}, true, nil)
	returned := asyncCall(c, context.Background())
	<-entered
	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for blocked Write")
	}
	if err := <-returned; err == nil {
		t.Fatal("blocked call succeeded")
	}
	_ = peer.Close()
}

func TestLateResponseDoesNotBlockAndRequestIDsIncrease(t *testing.T) {
	local, peer := net.Pipe()
	requests := make(chan *Request, 1)
	c := New(local, true, func(_ context.Context, request *Request) { requests <- request })
	ctx, cancel := context.WithCancel(context.Background())
	returned := asyncCall(c, ctx)
	lost := readFrame(t, peer)
	cancel()
	if !errors.Is(<-returned, context.Canceled) {
		t.Fatal("cancelled call did not return context error")
	}
	go func() {
		body, _ := protocol.ResultBytes(lost.ID, "session.list", protocol.SessionListResult{Sessions: []protocol.SessionSummary{}})
		_, _ = peer.Write(body)
		body, _ = protocol.RequestBytes(1, "session.open", protocol.OpenRequest{Name: "lane@local", Groups: []string{}, Open: protocol.OpenOptions{}})
		_, _ = peer.Write(body)
	}()
	select {
	case request := <-requests:
		if request.ID != 1 {
			t.Fatalf("request id = %d", request.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("reader blocked on late response")
	}
	body, _ := protocol.RequestBytes(1, "session.open", protocol.OpenRequest{Name: "again@local", Groups: []string{}, Open: protocol.OpenOptions{}})
	go peer.Write(body)
	<-c.Done()
}

func TestEOFFailsPendingOnce(t *testing.T) {
	local, peer := net.Pipe()
	c := New(local, true, nil)
	returned := asyncCall(c, context.Background())
	_ = readFrame(t, peer)
	_ = peer.Close()
	if err := <-returned; err == nil {
		t.Fatal("pending call succeeded after EOF")
	}
	<-c.Done()
}

func TestCloseIsIdempotent(t *testing.T) {
	local, peer := net.Pipe()
	counted := &countCloseConn{Conn: local}
	c := New(counted, true, nil)
	returned := asyncCall(c, context.Background())
	_ = readFrame(t, peer)
	var group sync.WaitGroup
	group.Add(2)
	for range 2 {
		go func() { defer group.Done(); _ = c.Close() }()
	}
	group.Wait()
	if counted.closes.Load() != 1 || <-returned == nil {
		t.Fatalf("closes = %d", counted.closes.Load())
	}
	_ = peer.Close()
}

func TestOversizeFrameClosesAtBound(t *testing.T) {
	local, peer := net.Pipe()
	c := New(local, true, nil)
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(peer, strings.Repeat("x", protocol.MaxFrameBytes+64)); written <- err }()
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("oversize frame was scanned past the bound")
	}
	if <-written == nil {
		t.Fatal("oversize writer was not interrupted")
	}
}

func TestMalformedPendingResponseClosesAllCalls(t *testing.T) {
	local, peer := net.Pipe()
	c := New(local, true, nil)
	returned := make(chan error, 2)
	for range 2 {
		go func() {
			returned <- c.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{})
		}()
	}
	first, second := readFrame(t, peer), readFrame(t, peer)
	bad := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":null}\n", min(first.ID, second.ID))
	_, _ = io.WriteString(peer, bad)
	if <-returned == nil || <-returned == nil {
		t.Fatal("malformed response left a pending call")
	}
	<-c.Done()
}

func TestCloseCancelsAdmittedHandler(t *testing.T) {
	local, peer := net.Pipe()
	entered, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	var c *Conn
	c = New(local, true, func(ctx context.Context, request *Request) {
		close(entered)
		<-release
		cancelled <- errors.Is(ctx.Err(), context.Canceled) && errors.Is(c.Result(request, protocol.OpenResult{SessionID: "late"}), ErrClosed)
	})
	go func() {
		body, _ := protocol.RequestBytes(1, "session.open", protocol.OpenRequest{Name: "lane@local", Groups: []string{}, Open: protocol.OpenOptions{}})
		_, _ = peer.Write(body)
	}()
	<-entered
	_ = c.Close()
	close(release)
	if !<-cancelled {
		t.Fatal("handler was not cancelled or wrote after close")
	}
}

func TestWriteErrorAndCloseShareTerminalPath(t *testing.T) {
	local, peer := net.Pipe()
	entered, release := make(chan struct{}), make(chan struct{})
	counted := &failingWriteConn{Conn: local, entered: entered, release: release}
	c := New(counted, true, nil)
	returned := asyncCall(c, context.Background())
	<-entered
	go c.Close()
	close(release)
	if <-returned == nil {
		t.Fatal("write failure succeeded")
	}
	<-c.Done()
	if counted.closes.Load() != 1 {
		t.Fatalf("closes = %d", counted.closes.Load())
	}
	_ = peer.Close()
}

type pauseWriteConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

func (c *pauseWriteConn) Write(body []byte) (int, error) {
	close(c.entered)
	<-c.release
	return c.Conn.Write(body)
}

type signalWriteConn struct {
	net.Conn
	entered chan struct{}
}

func (c *signalWriteConn) Write(body []byte) (int, error) {
	close(c.entered)
	return c.Conn.Write(body)
}

type countCloseConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *countCloseConn) Close() error { c.closes.Add(1); return c.Conn.Close() }

type failingWriteConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	closes  atomic.Int32
}

func (c *failingWriteConn) Write([]byte) (int, error) {
	close(c.entered)
	<-c.release
	return 0, io.ErrClosedPipe
}

func (c *failingWriteConn) Close() error { c.closes.Add(1); return c.Conn.Close() }

func readFrame(t *testing.T, connection net.Conn) protocol.Frame {
	t.Helper()
	body, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.DecodeFrame(body[:len(body)-1])
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func asyncCall(c *Conn, ctx context.Context) <-chan error {
	returned := make(chan error, 1)
	go func() {
		returned <- c.Call(ctx, "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{})
	}()
	return returned
}
