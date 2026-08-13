package federator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRewriteInboundFrameUsesLocalShadowReplyAddress(t *testing.T) {
	frame := json.RawMessage(`{"msgV":1,"msg_id":"m1","type":"user","session_id":"remote_target","from":"uds:/sender.sock","message":{"role":"user","content":"<cross-session-message from=\"uds:/sender.sock\" from-session=\"source-session\" from-name=\"source\" from-mode=\"prompting\">\nhello\n</cross-session-message>"}}`)
	target := localPeer{Peer: Peer{SessionID: "target-local"}}
	source := Peer{GlobalID: "host_b_source", DisplayName: "source--host-b"}
	got, err := rewriteInboundFrame(frame, target, source, "/tmp/path with space/shadow.sock")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(got, &value); err != nil {
		t.Fatal(err)
	}
	if value["from"] != "uds:/tmp/path%20with%20space/shadow.sock" {
		t.Fatalf("from = %v", value["from"])
	}
	if value["session_id"] != "target-local" {
		t.Fatalf("session_id = %v", value["session_id"])
	}
	content := value["message"].(map[string]any)["content"].(string)
	for _, want := range []string{
		`from="uds:/tmp/path%20with%20space/shadow.sock"`,
		`from-session="host_b_source"`,
		`from-name="source--host-b"`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("content does not contain %q: %s", want, content)
		}
	}
}

func TestRewriteInboundFramePreservesLargeJSONNumbers(t *testing.T) {
	frame := json.RawMessage(`{"type":"user","from":"uds:/sender.sock","sequence":9007199254740993,"message":{"content":"hello","sent_at_ns":9223372036854775807}}`)
	target := localPeer{Peer: Peer{SessionID: "target-local"}}
	source := Peer{GlobalID: "source-session", DisplayName: "source--remote"}
	got, err := rewriteInboundFrame(frame, target, source, "/tmp/shadow.sock")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(got))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	if got := value["sequence"].(json.Number).String(); got != "9007199254740993" {
		t.Fatalf("sequence was rounded to %s", got)
	}
	message := value["message"].(map[string]any)
	if got := message["sent_at_ns"].(json.Number).String(); got != "9223372036854775807" {
		t.Fatalf("nested timestamp was rounded to %s", got)
	}
}

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
