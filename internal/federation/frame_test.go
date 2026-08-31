package federation

import "testing"

func TestDeliveryFramePreservesNeutralProvenanceAndRequestIdentity(t *testing.T) {
	source := Peer{ID: "host-a/source", SessionID: "source", Name: "writer", Product: "codex", Groups: []string{"team"}}
	request := AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "message-1",
		Content: "body", Summary: "summary", SentAt: "2026-08-30T00:00:00Z",
	}
	delivery := DeliveryFrame(request, source)
	if delivery.Type != "delivery" || delivery.MessageID != request.MessageID || delivery.Content != "body" ||
		delivery.Source == nil || delivery.Source.ID != source.ID || delivery.SourceSessionID != source.SessionID ||
		delivery.SentAt != request.SentAt {
		t.Fatalf("delivery frame = %+v", delivery)
	}
	delivery.Source.Groups[0] = "changed"
	if source.Groups[0] != "team" {
		t.Fatal("delivery frame aliased source membership")
	}
}
