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
			check(t, frame.ID == id, "id = %d, want %d", frame.ID, id)
			body, _ := protocol.ResultBytes(id, "session.list", protocol.SessionListResult{Sessions: []protocol.SessionSummary{}})
			_, _ = peer.Write(body)
			if id == 1 {
				_, _ = peer.Write(body)
			}
		}
	}()
	for range 2 {
		var result protocol.SessionListResult
		must(t, c.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &result))
	}
	c.next = protocol.MaxRequestID
	check(t, c.Call(context.Background(), "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{}) != nil, "exhausted request id succeeded")
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
			must(t, <-done)
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
	check(t, <-returned != nil, "call succeeded")
	_ = peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	body, _ := bufio.NewReader(peer).ReadBytes('\n')
	check(t, len(body) == 0, "wrote %q", body)
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
	check(t, <-returned != nil, "blocked call succeeded")
	check(t, blocked.closes.Load() == 1, "closes = %d", blocked.closes.Load())
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
	check(t, errors.Is(<-returned, context.Canceled), "cancelled call did not return context error")
	go func() {
		body, _ := protocol.ResultBytes(lost.ID, "session.list", protocol.SessionListResult{Sessions: []protocol.SessionSummary{}})
		_, _ = peer.Write(body)
		body = []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\",\"method\":\"session.open\",\"params\":{\"name\":\"lane@local\",\"groups\":[],\"open\":{}}}\n")
		_, _ = peer.Write(body)
	}()
	select {
	case request := <-requests:
		check(t, request.ID == 1, "request id = %d", request.ID)
	case <-time.After(time.Second):
		t.Fatal("reader blocked on late response")
	}
	bad := []byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"session.open\",\"params\":{\"name\":\"\xff\",\"groups\":[],\"open\":{}}}\n")
	good, _ := protocol.RequestBytes(3, "session.open", protocol.OpenRequest{Name: "again@local", Groups: []string{}, Open: protocol.OpenOptions{}})
	go peer.Write(append(bad, good...))
	rejected := readFrame(t, peer)
	check(t, rejected.Error != nil && rejected.Error.Code == protocol.InvalidFrame, "invalid response = %#v", rejected)
	<-c.Done()
	select {
	case <-requests:
		t.Fatal("frame after invalid input reached handler")
	default:
	}
}

func TestInvalidRequestRepliesThenCloses(t *testing.T) {
	checkRejected := func(body string, code int) {
		local, peer := net.Pipe()
		New(local, false, nil)
		go peer.Write(append([]byte(body), '\n'))
		reader := bufio.NewReader(peer)
		if code != 0 {
			want, _ := protocol.ErrorBytes(1, code, nil)
			got, err := reader.ReadBytes('\n')
			check(t, err == nil && string(got) == string(want), "response = %q, %v; want %q", got, err, want)
		}
		tail, err := reader.ReadBytes('\n')
		check(t, len(tail) == 0 && errors.Is(err, io.EOF), "after response = %q, %v; want EOF", tail, err)
	}
	checkRejected(`{"jsonrpc":"2.0","id":1,"method":"session.list","params":{"extra":true}}`, protocol.InvalidFrame)
	checkRejected(`{"jsonrpc":"2.0","id":1,"method":"session.hello","params":{"protocol":1,"product":"p","session_id":"s","name":"n","groups":[],"info":{},"launch_token":"t","supported_open_fields":[],"extra_arguments":[]}}`, protocol.InvalidHello)
	checkRejected(`{`, 0)
	for _, method := range []string{`"method":null,`, `"method":"",`, `"method":1,`, ``} {
		checkRejected(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,%s"params":{}}`, method), protocol.InvalidFrame)
	}
}

func TestEOFFailsPendingOnce(t *testing.T) {
	local, peer := net.Pipe()
	c := New(local, true, nil)
	returned := asyncCall(c, context.Background())
	_ = readFrame(t, peer)
	_, _ = peer.Write([]byte("{"))
	_ = peer.Close()
	check(t, <-returned != nil, "pending call succeeded after EOF")
}

func TestCloseIsIdempotent(t *testing.T) {
	local, peer := net.Pipe()
	counted := &signalWriteConn{Conn: local, entered: make(chan struct{})}
	c := New(counted, true, nil)
	returned := asyncCall(c, context.Background())
	_ = readFrame(t, peer)
	var group sync.WaitGroup
	group.Add(2)
	for range 2 {
		go func() { defer group.Done(); _ = c.Close() }()
	}
	group.Wait()
	check(t, counted.closes.Load() == 1 && <-returned != nil, "closes = %d", counted.closes.Load())
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
	check(t, <-written != nil, "oversize writer was not interrupted")
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
	check(t, <-returned != nil && <-returned != nil, "malformed response left a pending call")
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
	check(t, <-cancelled, "handler was not cancelled or wrote after close")
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
		must(t, err)
		_, err = protocol.DecodeFrame(body[:len(body)-1])
		must(t, err)
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
	must(t, err)
	frame, err := protocol.DecodeFrame(body[:len(body)-1])
	must(t, err)
	return frame
}

func asyncCall(c *Conn, ctx context.Context) <-chan error {
	returned := make(chan error, 1)
	go func() {
		returned <- c.Call(ctx, "session.list", protocol.SessionListRequest{}, &protocol.SessionListResult{})
	}()
	return returned
}

func must(t *testing.T, err error) { check(t, err == nil, "%v", err) }
func check(t *testing.T, ok bool, format string, args ...any) {
	if !ok {
		t.Fatalf(format, args...)
	}
}
