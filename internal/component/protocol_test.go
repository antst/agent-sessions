package component

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeFrameV1AcceptsAdditiveFieldsAndRejectsUnknownAuthority(t *testing.T) {
	body := []byte(`{
		"version":1,"type":"session.announce","id":"announce-1","seq":2,
		"payload":{"binding_id":"binding","native_session_id":"native-not-attachment","cwd":"/work","native_name":"name","product_event_seq":4},
		"future_diagnostic":"ignored"
	}`)
	frame, err := DecodeFrame(body)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	var announce SessionAnnounce
	if err := frame.PayloadInto(&announce); err != nil {
		t.Fatalf("PayloadInto: %v", err)
	}
	if announce.NativeSessionID != "native-not-attachment" || announce.ProductEventSeq != 4 {
		t.Fatalf("announce payload = %#v", announce)
	}

	for _, body := range []string{
		`{"version":2,"type":"heartbeat","id":"x","seq":1,"payload":{}}`,
		`{"version":1,"type":"future.authority","id":"x","seq":1,"payload":{}}`,
		`{"version":1,"type":"heartbeat","id":"","seq":1,"payload":{}}`,
		`{"version":1,"type":"heartbeat","id":"x","seq":0,"payload":{}}`,
	} {
		if _, err := DecodeFrame([]byte(body)); err == nil {
			t.Fatalf("DecodeFrame accepted %s", body)
		}
	}
}

func TestProtocolPayloadValidation(t *testing.T) {
	valid := map[FrameType]any{
		TypeBootstrap:      BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: "raw-secret", ProcessStart: "10", StrongStart: "10", ComponentVersion: "1.2.3"},
		TypeReconnect:      ReconnectClaim{AttachmentID: "attachment", PriorBindingID: "binding-old", PriorGeneration: 4, ProcessStart: "10", StrongStart: "10", LastReceivedSeq: 9},
		TypeHeartbeat:      Heartbeat{BindingID: "binding", LastReceivedSeq: 2},
		TypeDeliveryAccept: DeliveryAccept{DeliveryID: "delivery", NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 1},
		TypeToolCall:       ToolCall{CallID: "call", Operation: "sessions.send", Arguments: json.RawMessage(`{"target":"peer"}`)},
	}
	for frameType, payload := range valid {
		frame, err := NewFrame(frameType, "operation", 1, payload)
		if err != nil {
			t.Fatalf("NewFrame(%s): %v", frameType, err)
		}
		if err := ValidatePayload(frame); err != nil {
			t.Fatalf("ValidatePayload(%s): %v", frameType, err)
		}
	}

	frame, err := NewFrame(TypeBootstrap, "bootstrap", 1, BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: "secret", ProcessStart: "10"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayload(frame); err == nil {
		t.Fatal("bootstrap without strong process start was accepted")
	}
}

func TestRedactRemovesRawBootstrapAndSensitiveDiagnostics(t *testing.T) {
	secret := "correct horse battery staple"
	got := Redact(`bootstrap_value=`+secret+` password=hunter2 token: abc123 harmless=ok`, secret)
	for _, forbidden := range []string{secret, "hunter2", "abc123"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("redacted detail contains %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "harmless=ok") || !strings.Contains(got, RedactedValue) {
		t.Fatalf("redacted detail = %q", got)
	}
}

func TestProtocolErrorHasStableCategory(t *testing.T) {
	_, err := DecodeFrame([]byte(`{"version":99,"type":"heartbeat","id":"x","seq":1,"payload":{}}`))
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Category != CategoryUnsupportedVersion {
		t.Fatalf("protocol error = %#v", err)
	}
}

func FuzzDecodeFrameV1(f *testing.F) {
	f.Add([]byte(`{"version":1,"type":"heartbeat","id":"heartbeat","seq":1,"payload":{"binding_id":"binding","last_received_seq":0}}`))
	f.Add([]byte(`{"version":999,"type":"unknown","id":"x","seq":1,"payload":{}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		frame, err := DecodeFrame(body)
		if err != nil {
			return
		}
		encoded, err := EncodeFrame(frame)
		if err != nil {
			return
		}
		if _, err := DecodeFrame(encoded); err != nil {
			t.Fatalf("accepted frame did not round trip: %v", err)
		}
	})
}
