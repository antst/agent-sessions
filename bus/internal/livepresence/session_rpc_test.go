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
