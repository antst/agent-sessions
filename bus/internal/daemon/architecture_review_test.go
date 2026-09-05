package daemon

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/bus/internal/conn"
	"github.com/antst/agent-sessions/bus/internal/protocol"
)

func reviewSession(t *testing.T) (*Daemon, *session, net.Conn) {
	t.Helper()
	d := &Daemon{}
	d.directory = newDirectory(d, nil)
	s := newSession(d)
	local, peer := net.Pipe()
	s.wire = conn.Start(local, s.inbox, &d.group)
	t.Cleanup(func() {
		s.wire.Close()
		_ = peer.Close()
		for event := range s.inbox {
			if _, ok := event.(conn.Closed); ok {
				break
			}
		}
		s.wire.OwnerClosed()
		d.group.Wait()
	})
	return d, s, peer
}

func TestReviewAliasesDeliverOnce(t *testing.T) {
	_, socket := startDaemon(t)
	sender := connectPeer(t, socket, "sender", "sender", "shared")
	one := connectPeer(t, socket, "one", "one", "shared")
	var result protocol.MessageSendResult
	must(t, sender.call("message.send", protocol.MessageSendRequest{Targets: []string{"one", "one@local"}, Message: "once"}, &result))
	if got := len(one.deliveries); got != 1 || len(result.Deliveries) != 1 {
		t.Fatalf("deliveries = %d, receipts = %d", got, len(result.Deliveries))
	}
}

func TestReviewRejectedRunLeavesIdle(t *testing.T) {
	d, s, _ := reviewSession(t)
	item := &entry{row: row{SessionID: "lane@local"}, attachment: s, done: make(chan struct{})}
	d.directory.entries[item.row.SessionID], s.identity = item, item
	for id := int64(1); id <= maxPendingCalls; id++ {
		s.pending[id] = routedRequest{}
	}
	reply := make(chan answer, 1)
	d.directory.mu.Lock()
	code := d.directory.routeLocked(item, "turn.run", routedRequest{method: "turn.run", reply: reply})
	d.directory.mu.Unlock()
	if code != 0 {
		t.Fatal(code)
	}
	s.handleEvent(<-s.inbox)
	if got := <-reply; got.code != protocol.Busy || item.running {
		t.Fatalf("answer = %#v, running = %v", got, item.running)
	}
}

func TestReviewAbortKeepsReservationThroughCleanup(t *testing.T) {
	d := &Daemon{}
	d.directory = newDirectory(d, nil)
	start := newLaunch("worker", false, true)
	defer start.timer.Stop()
	item, code := d.directory.reserveFresh(row{Name: "parent/lane@local"}, start)
	if code != 0 {
		t.Fatal(code)
	}
	defer d.group.Done()
	s := newSession(d)
	s.launch, s.identity = start, item
	start.owner = s
	s.abortLaunch(answer{code: protocol.SpawnFailed})
	if d.directory.names[item.row.Name] != item || !item.claimed {
		t.Fatal("abort released reservation before final cleanup")
	}
}

func TestReviewRehelloSettlesOldInboundDelivery(t *testing.T) {
	d, s, _ := reviewSession(t)
	hello := &protocol.PeerHello{Protocol: 1, Product: "peer", SessionID: "old", Name: "old", Groups: []string{"shared"}}
	item, _, _, ok := d.directory.installPeer(s, hello, "local")
	if !ok {
		t.Fatal("install")
	}
	s.identity = item
	reply := make(chan answer, 1)
	s.pending[1] = routedRequest{destination: item, method: "message.deliver", reply: reply}
	hello.SessionID, hello.Name = "new", "new"
	s.peerHello(protocol.Frame{ID: 2, Method: "session.hello", Request: true}, hello)
	if got := <-reply; got.code != protocol.NotConnected {
		t.Fatalf("answer = %#v", got)
	}
}

func TestReviewRunTerminalPrecedesCloseReply(t *testing.T) {
	_, s, peer := reviewSession(t)
	target := &entry{}
	s.requests[1] = &requestState{frame: protocol.Frame{ID: 1, Method: "turn.run"}, target: target}
	s.requests[2] = &requestState{frame: protocol.Frame{ID: 2, Method: "session.close"}, target: target}
	s.owned = 2
	s.consumeReply(replyEvent{requestID: 2, answer: answer{value: struct{}{}}})
	_ = peer.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("close reply was written before the run terminal")
	}
	_ = peer.SetReadDeadline(time.Time{})
	s.consumeReply(replyEvent{requestID: 1, answer: answer{value: &protocol.TurnResult{Outcome: "completed", Result: "done"}}})
	reader := bufio.NewReader(peer)
	readID := func() int64 {
		body, err := reader.ReadBytes('\n')
		must(t, err)
		frame, err := protocol.DecodeFrame(body[:len(body)-1])
		must(t, err)
		return frame.ID
	}
	if first, second := readID(), readID(); first != 1 || second != 2 {
		t.Fatalf("response order = %d, %d", first, second)
	}
}

func TestReviewInterruptedRowWriteDoesNotBlockRestart(t *testing.T) {
	path := t.TempDir()
	table, _, err := openTable(path)
	must(t, err)
	must(t, table.write(row{SessionID: "lane@local", Product: "worker", Name: "parent/lane@local", Groups: []string{"one", "two"}, CreatedAt: time.Now()}))
	must(t, os.WriteFile(filepath.Join(path, ".row-interrupted"), []byte("{"), 0o600))
	_, rows, err := openTable(path)
	must(t, err)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
}
