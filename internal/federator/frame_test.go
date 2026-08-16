package federator

import (
	"encoding/json"
	"testing"
)

func TestSourcePeerIDUsesOuterReplyAddress(t *testing.T) {
	frame := json.RawMessage(`{"type":"user","from":"uds:/tmp/a%20peer.sock"}`)
	local := map[string]localPeer{
		"a/session": {Peer: Peer{ID: "a/session"}, Socket: "/tmp/a peer.sock"},
	}
	got, err := sourcePeerID(frame, local)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a/session" {
		t.Fatalf("source = %q", got)
	}
}
