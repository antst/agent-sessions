package livepresence

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSessionRPCRejectsRecoverableFramesBeforeDispatch(t *testing.T) {
	cases := []struct{ name, frame string }{
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"unknown","params":{}}`},
		{"open envelope", `{"jsonrpc":"2.0","id":1,"method":"session.list","params":{},"extra":true}`},
		{"open params", `{"jsonrpc":"2.0","id":1,"method":"session.list","params":{"extra":true}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rpc, remote := sessionRPCPair(t)
			read := make(chan Frame, 1)
			go func() { frame, _ := rpc.Read(false); read <- frame }()
			writeSessionFrame(t, remote, test.frame)
			response := readSessionFrame(t, remote)
			if response.Error == nil || response.Error.Code != SessionInvalidFrame {
				t.Fatalf("response = %+v", response)
			}
			writeSessionFrame(t, remote, `{"jsonrpc":"2.0","id":2,"method":"session.list","params":{}}`)
			if frame := <-read; frame.Method != "session.list" || string(frame.ID) != "2" {
				t.Fatalf("dispatched frame = %+v", frame)
			}
		})
	}
}

func TestSessionRPCClosesOnUnrecoverableFrames(t *testing.T) {
	cases := []struct {
		name, frame string
		want        error
	}{
		{"malformed", `{"jsonrpc":`, nil},
		{"unsafe id", `{"jsonrpc":"2.0","id":9007199254740992,"method":"session.list","params":{}}`, nil},
		{"oversized", strings.Repeat("x", MaxFrameBytes+1), ErrFrameTooLarge},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rpc, remote := sessionRPCPair(t)
			written := make(chan struct{})
			go func() {
				_, _ = remote.Write(append([]byte(test.frame), '\n'))
				close(written)
			}()
			_, err := rpc.Read(false)
			if err == nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("read error = %v, want %v", err, test.want)
			}
			_ = rpc.Close()
			<-written
		})
	}
}

func TestSessionRPCLateResponseIsDrainedAfterCallerLeaves(t *testing.T) {
	rpc, remote := sessionRPCPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan error, 1)
	go func() { called <- rpc.Call(ctx, true, "session.list", struct{}{}, &struct{}{}) }()
	request := readSessionFrame(t, remote)
	cancel()
	if err := <-called; !errors.Is(err, context.Canceled) {
		t.Fatalf("call error = %v", err)
	}
	read := make(chan Frame, 1)
	go func() { frame, _ := rpc.Read(true); read <- frame }()
	writeSessionFrame(t, remote, `{"jsonrpc":"2.0","id":`+string(request.ID)+`,"result":{"sessions":[]}}`)
	writeSessionFrame(t, remote, `{"jsonrpc":"2.0","id":1,"method":"message.deliver","params":{"message_id":"m","from":{"session_id":"p@local","name":"p@local","product":"peer","groups":[]},"body":"body"}}`)
	if frame := <-read; frame.Method != "message.deliver" {
		t.Fatalf("frame after late response = %+v", frame)
	}
}

func TestSessionRPCDropsUnmatchedResponseWithoutWriting(t *testing.T) {
	rpc, remote := sessionRPCPair(t)
	read := make(chan Frame, 1)
	go func() { frame, _ := rpc.Read(true); read <- frame }()
	writeSessionFrame(t, remote, `{"jsonrpc":"2.0","id":77,"result":{"sessions":[]}}`)
	_ = remote.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := bufio.NewReader(remote).ReadBytes('\n'); err == nil {
		t.Fatal("unmatched response produced a reply")
	}
	_ = remote.SetReadDeadline(time.Time{})
	writeSessionFrame(t, remote, `{"jsonrpc":"2.0","id":1,"method":"message.deliver","params":{"message_id":"m","from":{"session_id":"p@local","name":"p@local","product":"peer","groups":[]},"body":"body"}}`)
	if frame := <-read; frame.Method != "message.deliver" {
		t.Fatalf("frame after unmatched response = %+v", frame)
	}
}

func TestSessionRPCCloseFailsPendingCallOnce(t *testing.T) {
	rpc, remote := sessionRPCPair(t)
	called := make(chan error, 1)
	go func() { called <- rpc.Call(context.Background(), true, "session.list", struct{}{}, &struct{}{}) }()
	_ = readSessionFrame(t, remote)
	if err := rpc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-called; err == nil {
		t.Fatal("pending call succeeded after close")
	}
	if err := rpc.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestSessionRPCCloseWinsBeforeAPausedCallWrites(t *testing.T) {
	local, remote := net.Pipe()
	blocked := &blockingCloseConn{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
	rpc, err := NewSessionRPC(blocked)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	beforeWrite, releaseWrite := make(chan struct{}), make(chan struct{})
	rpc.testBeforeWrite = func() { close(beforeWrite); <-releaseWrite }
	called := make(chan error, 1)
	go func() { called <- rpc.Call(context.Background(), true, "session.list", struct{}{}, &struct{}{}) }()
	<-beforeWrite
	closed := make(chan error, 1)
	go func() { closed <- rpc.Close() }()
	<-blocked.entered
	close(releaseWrite)
	if err := <-called; err == nil {
		t.Fatal("call succeeded after terminal mark")
	}
	_ = remote.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := bufio.NewReader(remote).ReadByte(); err == nil {
		t.Fatal("paused call wrote after terminal mark")
	}
	close(blocked.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestSessionRPCTerminalMarkRejectsLateWorkBeforeCloseReturns(t *testing.T) {
	local, remote := net.Pipe()
	blocked := &blockingCloseConn{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
	rpc, err := NewSessionRPC(blocked)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = remote.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"message.deliver","params":{"message_id":"m","from":{"session_id":"p@local","name":"p@local","product":"peer","groups":[]},"body":"body"}}` + "\n"))
	}()
	request, err := rpc.Read(true)
	if err != nil {
		t.Fatal(err)
	}
	<-written
	pending := make(chan error, 1)
	go func() { pending <- rpc.Call(context.Background(), true, "session.list", struct{}{}, &struct{}{}) }()
	_ = readSessionFrame(t, remote)
	closed := make(chan error, 1)
	go func() { closed <- rpc.Close() }()
	<-blocked.entered
	if err := rpc.Call(context.Background(), true, "session.list", struct{}{}, &struct{}{}); err == nil {
		t.Fatal("call registered after terminal mark")
	}
	if err := rpc.Result(request, SessionDeliveryReceipt{Disposition: "injected"}); err == nil {
		t.Fatal("response written after terminal mark")
	}
	if rpc.resolve("999", Success(json.RawMessage("999"), json.RawMessage(`{}`))) {
		t.Fatal("response resolved after terminal mark")
	}
	if err := rpc.wire.Write(Success(json.RawMessage("2"), json.RawMessage(`{}`))); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("connection write after terminal mark = %v", err)
	}
	close(blocked.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-pending; err == nil {
		t.Fatal("preexisting pending call survived close")
	}
}

func TestSessionRPCDuplicateMembersUseTheLastValue(t *testing.T) {
	rpc, remote := sessionRPCPair(t)
	read := make(chan Frame, 1)
	go func() { frame, _ := rpc.Read(false); read <- frame }()
	writeSessionFrame(t, remote, `{"jsonrpc":"2.0","id":1,"method":"unknown","method":"session.list","params":{"extra":true},"params":{}}`)
	if frame := <-read; frame.Method != "session.list" || string(frame.Params) != `{}` {
		t.Fatalf("last members did not win: %+v", frame)
	}
}

func sessionRPCPair(t *testing.T) (*SessionRPC, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	rpc, err := NewSessionRPC(local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rpc.Close(); _ = remote.Close() })
	return rpc, remote
}

func writeSessionFrame(t *testing.T, connection net.Conn, frame string) {
	t.Helper()
	if _, err := connection.Write(append([]byte(frame), '\n')); err != nil {
		t.Fatal(err)
	}
}

func readSessionFrame(t *testing.T, connection net.Conn) Frame {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	body, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var frame Frame
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

type blockingCloseConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

func (c *blockingCloseConn) Close() error {
	close(c.entered)
	<-c.release
	return c.Conn.Close()
}
