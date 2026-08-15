package bridge

import "testing"

func TestParsePeerMessageExtensions(t *testing.T) {
	got := parsePeerMessage("<cross-session-message from=\"uds:/tmp/a.sock\" from-session=\"s1\" from-name=\"peer-a\" from-mode=\"bypass\">\n[codex-peer-metadata: {\"fromProduct\":\"codex\",\"messageId\":\"m1\",\"sentAt\":\"now\"}]\nhello\n</cross-session-message>")
	if got.FromName != "peer-a" || got.FromProduct != "codex" || got.MessageID != "m1" || got.Message != "hello" {
		t.Fatalf("unexpected envelope: %#v", got)
	}
}

func TestAdditionalProductEnvelopeRoundTrip(t *testing.T) {
	content := wrapNativePeerMessageFromProduct(
		"uds:/tmp/agy.sock", "conversation-id", "agy-review", "bypass",
		"message-id", "now", "agy", "hello",
	)
	got := parsePeerMessage(content)
	if got.FromProduct != "agy" || got.FromSession != "conversation-id" || got.Message != "hello" {
		t.Fatalf("unexpected Antigravity envelope: %#v", got)
	}
	if peerProductDisplayName(got.FromProduct) != "Antigravity" || normalizePeerProduct("unknown") != "" {
		t.Fatal("additional product label or closed allowlist is inconsistent")
	}
}

func TestPeerNameSanitization(t *testing.T) {
	if got := sanitizeName(" Reviewer / lane "); got != "Reviewer-lane" {
		t.Fatalf("got %q", got)
	}
}
