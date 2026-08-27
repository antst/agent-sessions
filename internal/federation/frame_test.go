package federation

import "testing"

func TestDecodeAgentFrameBodyOwnsTheSharedCarrierContract(t *testing.T) {
	frame, err := DecodeAgentFrameBody(`<cross-session-message from="agent">
AGENT_SESSIONS_FRAME {"version":1,"type":"send","message_id":"m-1","content":"hello"}
</cross-session-message>`)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Version != AgentFrameVersion || frame.Type != "send" || frame.MessageID != "m-1" || frame.Content != "hello" {
		t.Fatalf("decoded frame = %+v", frame)
	}
}

func TestAgentFrameResultUsesSharedPeerIdentity(t *testing.T) {
	result := AgentFrameResult{Version: AgentFrameVersion, Type: "discover.result", Peers: []Peer{{ID: "host/session"}}}
	if len(result.Peers) != 1 || result.Peers[0].ID != "host/session" {
		t.Fatalf("result = %+v", result)
	}
}
