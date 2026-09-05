package rpc

import (
	"bufio"
	"context"
	"encoding/json"
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
			if id == 1 {
				_, _ = peer.Write(body)
			}
		}
	}()
	for range 2 {
		var result protocol.SessionListResult
		if err := c.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &result); err != nil {
			t.Fatal(err)
		}
	}
	c.next = protocol.MaxRequestID
	if err := c.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}); err == nil {
		t.Fatal("exhausted request id succeeded")
	}
}

func TestResponseClaimsTargetBeforeCancelOrClose(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			local, peer := net.Pipe()
			c, ctx := New(local, true, nil), context.Background()
			var cancel context.CancelFunc
			if action == "cancel" {
				ctx, cancel = context.WithCancel(ctx)
			}
			target := &pausedResult{entered: make(chan struct{}), release: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- c.Call(ctx, "session.list", protocol.SessionListRequest{}, target) }()
			request := readFrame(t, peer)
			body, _ := protocol.ResultBytes(request.ID, "session.list", protocol.SessionListResult{Sessions: []protocol.SessionSummary{}})
			go peer.Write(body)
			<-target.entered
			if cancel != nil {
				cancel()
			} else {
				_ = c.Close()
			}
			select {
			case <-done:
				t.Fatal("call returned while its target was decoding")
			default:
			}
			close(target.release)
			if err := <-done; err != nil {
				t.Fatalf("claimed response lost to %s: %v", action, err)
			}
			_ = peer.Close()
		})
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
	blocked := &signalWriteConn{Conn: local, entered: entered}
	c := New(blocked, true, nil)
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
	if blocked.closes.Load() != 1 {
		t.Fatalf("closes = %d", blocked.closes.Load())
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
		body = []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\",\"method\":\"session.open\",\"params\":{\"name\":\"lane@local\",\"groups\":[],\"open\":{}}}\n")
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
	bad := []byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"session.open\",\"params\":{\"name\":\"\xff\",\"groups\":[],\"open\":{}}}\n")
	good, _ := protocol.RequestBytes(3, "session.open", protocol.OpenRequest{Name: "again@local", Groups: []string{}, Open: protocol.OpenOptions{}})
	go peer.Write(append(bad, good...))
	<-c.Done()
	select {
	case <-requests:
		t.Fatal("frame after invalid input reached handler")
	default:
	}
}

func TestEOFFailsPendingOnce(t *testing.T) {
	local, peer := net.Pipe()
	c := New(local, true, nil)
	returned := asyncCall(c, context.Background())
	_ = readFrame(t, peer)
	_, _ = peer.Write([]byte("{"))
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

func TestConcurrentHandlersWriteCompleteFrames(t *testing.T) {
	local, peer := net.Pipe()
	ready, release := sync.WaitGroup{}, make(chan struct{})
	ready.Add(2)
	var c *Conn
	c = New(local, true, func(_ context.Context, request *Request) { ready.Done(); <-release; _ = c.Result(request, struct{}{}) })
	first, _ := protocol.RequestBytes(1, "session.superseded", struct{}{})
	second, _ := protocol.RequestBytes(2, "session.superseded", struct{}{})
	go peer.Write(append(first, second...))
	ready.Wait()
	close(release)
	reader := bufio.NewReader(peer)
	for range 2 {
		body, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if _, err = protocol.DecodeFrame(body[:len(body)-1]); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close()
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
	closes  atomic.Int32
}

func (c *signalWriteConn) Write(body []byte) (int, error) {
	close(c.entered)
	return c.Conn.Write(body)
}
func (c *signalWriteConn) Close() error { c.closes.Add(1); return c.Conn.Close() }

type countCloseConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *countCloseConn) Close() error { c.closes.Add(1); return c.Conn.Close() }

type pausedResult struct {
	protocol.SessionListResult
	entered, release chan struct{}
}

func (r *pausedResult) UnmarshalJSON(raw []byte) error {
	close(r.entered)
	<-r.release
	return json.Unmarshal(raw, &r.SessionListResult)
}

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
