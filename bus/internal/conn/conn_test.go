package conn

import (
	"bufio"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

func TestReaderPostsFramesAndOneClose(t *testing.T) {
	local, peer := net.Pipe()
	var group sync.WaitGroup
	c := Start(local, &group)
	body, _ := protocol.RequestBytes(1, "session.list", protocol.SessionListRequest{})
	go func() { _, _ = peer.Write(body) }()
	event := (<-c.Inbox()).(Frame)
	if event.Err != nil || event.Value.ID != 1 || event.Value.Method != "session.list" {
		t.Fatalf("frame = %#v", event)
	}
	_ = peer.Close()
	closed := (<-c.Inbox()).(Closed)
	if closed.Connection != c {
		t.Fatalf("closed connection = %p, want %p", closed.Connection, c)
	}
	select {
	case extra := <-c.Inbox():
		t.Fatalf("second terminal event = %#v", extra)
	case <-time.After(20 * time.Millisecond):
	}
	group.Wait()
}

func TestReaderPostBlocksUntilOwnerReceives(t *testing.T) {
	local, peer := net.Pipe()
	var group sync.WaitGroup
	c := Start(local, &group)
	first, _ := protocol.RequestBytes(1, "session.list", protocol.SessionListRequest{})
	second, _ := protocol.RequestBytes(2, "session.list", protocol.SessionListRequest{})
	written := make(chan struct{})
	go func() { _, _ = peer.Write(append(first, second...)); close(written) }()
	<-written
	if event := (<-c.Inbox()).(Frame); event.Value.ID != 1 {
		t.Fatalf("first frame id = %d", event.Value.ID)
	}
	if event := (<-c.Inbox()).(Frame); event.Value.ID != 2 {
		t.Fatalf("second frame id = %d", event.Value.ID)
	}
	c.Close()
	<-c.Inbox()
	group.Wait()
	_ = peer.Close()
}

func TestWriterPreservesFramesAndFinalCloses(t *testing.T) {
	local, peer := net.Pipe()
	var group sync.WaitGroup
	c := Start(local, &group)
	first, _ := protocol.ResultBytes(1, "session.hello", struct{}{})
	last, _ := protocol.RequestBytes(1, "session.superseded", struct{}{})
	if !c.Send(first) || !c.Finish(last, time.Second) {
		t.Fatal("writer refused frames")
	}
	reader := bufio.NewReader(peer)
	for _, want := range [][]byte{first, last} {
		got, err := reader.ReadBytes('\n')
		if err != nil || string(got) != string(want) {
			t.Fatalf("frame = %q, %v; want %q", got, err, want)
		}
	}
	if tail, err := reader.ReadBytes('\n'); len(tail) != 0 || err != io.EOF {
		t.Fatalf("tail = %q, %v", tail, err)
	}
	if closed := (<-c.Inbox()).(Closed); closed.Connection != c {
		t.Fatalf("closed = %#v", closed)
	}
	group.Wait()
	_ = peer.Close()
}

func TestSlowWriterClosesAtOutboxBound(t *testing.T) {
	local, peer := net.Pipe()
	var group sync.WaitGroup
	c := Start(local, &group)
	frame, _ := protocol.ResultBytes(1, "session.hello", struct{}{})
	accepted := 0
	for c.Send(frame) {
		accepted++
	}
	if accepted < OutboxSize || accepted > OutboxSize+1 {
		t.Fatalf("accepted = %d", accepted)
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("slow writer did not close")
	}
	if closed := (<-c.Inbox()).(Closed); closed.Connection != c {
		t.Fatalf("closed = %#v", closed)
	}
	group.Wait()
	_ = peer.Close()
}

func TestInvalidFrameIsPostedBeforeClose(t *testing.T) {
	local, peer := net.Pipe()
	var group sync.WaitGroup
	c := Start(local, &group)
	go func() { _, _ = io.WriteString(peer, "{\n") }()
	event := (<-c.Inbox()).(Frame)
	if event.Err == nil {
		t.Fatal("invalid frame was accepted")
	}
	c.Close()
	if closed := (<-c.Inbox()).(Closed); closed.Connection != c {
		t.Fatalf("closed = %#v", closed)
	}
	group.Wait()
	_ = peer.Close()
}

func TestOversizeFrameHardCloses(t *testing.T) {
	local, peer := net.Pipe()
	var group sync.WaitGroup
	c := Start(local, &group)
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(peer, string(make([]byte, protocol.MaxFrameBytes+1))); written <- err }()
	closed := (<-c.Inbox()).(Closed)
	if closed.Connection != c || <-written == nil {
		t.Fatalf("oversize close = %#v", closed)
	}
	group.Wait()
	_ = peer.Close()
}
