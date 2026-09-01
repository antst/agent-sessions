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
		TypeBootstrap:            BootstrapClaim{ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability", BootstrapValue: "raw-secret", ProcessStart: "10", StrongStart: "10", ComponentVersion: ContractRevision},
		TypeReconnect:            ReconnectClaim{AttachmentID: "attachment", PriorBindingID: "binding-old", PriorGeneration: 4, ProcessStart: "10", StrongStart: "10", LastReceivedSeq: 9},
		TypeHeartbeat:            Heartbeat{BindingID: "binding", LastReceivedSeq: 2},
		TypeDeliveryAccept:       DeliveryAccept{DeliveryID: "delivery", NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 1},
		TypeToolCall:             ToolCall{CallID: "call", Operation: "sessions.send", Arguments: json.RawMessage(`{"target":"peer"}`)},
		TypeSessionRenameRequest: SessionRenameRequest{NativeSessionID: "native", RequestedName: "new title"},
	}
	for frameType, payload := range valid {
		operationID := "operation"
		if frameType == TypeSessionRenameRequest {
			operationID = DaemonRenameOperationPrefix + "operation"
		}
		frame, err := NewFrame(frameType, operationID, 1, payload)
		if err != nil {
			t.Fatalf("NewFrame(%s): %v", frameType, err)
		}
		if err := ValidatePayload(frame); err != nil {
			t.Fatalf("ValidatePayload(%s): %v", frameType, err)
		}
	}
	for _, test := range []struct {
		name    string
		payload any
	}{
		{name: "bootstrap", payload: BootstrapClaim{
			ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability",
			BootstrapValue: "raw-secret", ComponentVersion: ContractRevision,
		}},
		{name: "reconnect", payload: ReconnectClaim{
			AttachmentID: "attachment", PriorBindingID: "binding-old", PriorGeneration: 4, LastReceivedSeq: 9,
		}},
	} {
		t.Run(test.name+" omitted process corroboration", func(t *testing.T) {
			frameType := TypeBootstrap
			if test.name == "reconnect" {
				frameType = TypeReconnect
			}
			frame, err := NewFrame(frameType, "operation", 1, test.payload)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(frame.Payload), "process_start") || strings.Contains(string(frame.Payload), "strong_start") {
				t.Fatalf("omitted process corroboration was serialized: %s", frame.Payload)
			}
			if err := ValidatePayload(frame); err != nil {
				t.Fatalf("omitted process corroboration: %v", err)
			}
		})
	}

	for _, test := range []struct {
		name      string
		frameType FrameType
		payload   any
	}{
		{name: "bootstrap process-start only", frameType: TypeBootstrap, payload: BootstrapClaim{
			ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability",
			BootstrapValue: "secret", ProcessStart: "10", ComponentVersion: ContractRevision,
		}},
		{name: "bootstrap strong-start only", frameType: TypeBootstrap, payload: BootstrapClaim{
			ProductID: "pi", AttachmentID: "attachment", BootstrapCapabilityID: "capability",
			BootstrapValue: "secret", StrongStart: "strong", ComponentVersion: ContractRevision,
		}},
		{name: "reconnect process-start only", frameType: TypeReconnect, payload: ReconnectClaim{
			AttachmentID: "attachment", PriorBindingID: "binding-old", PriorGeneration: 4, ProcessStart: "10",
		}},
		{name: "reconnect strong-start only", frameType: TypeReconnect, payload: ReconnectClaim{
			AttachmentID: "attachment", PriorBindingID: "binding-old", PriorGeneration: 4, StrongStart: "strong",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, err := NewFrame(test.frameType, "operation", 1, test.payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayload(frame); err == nil {
				t.Fatal("partial process corroboration was accepted")
			}
		})
	}
}

func TestRenameFrameNamespacesAndContractRevision(t *testing.T) {
	if ProtocolVersion != 1 || ContractRevision != "agent-sessions.component.v1-r1" {
		t.Fatalf("component contract = wire %d / %q", ProtocolVersion, ContractRevision)
	}
	if err := ValidateContractRevision(ContractRevision); err != nil {
		t.Fatalf("ValidateContractRevision(current): %v", err)
	}
	if err := ValidateContractRevision("agent-sessions.component.v1-r0"); err == nil {
		t.Fatal("doctor seam accepted a mismatched pinned component contract")
	}
	if len(knownFrameTypes) != 21 {
		t.Fatalf("frozen component frame vocabulary = %d, want 21", len(knownFrameTypes))
	}
	daemonID, err := DaemonRenameOperationID("stable-operation")
	if err != nil || daemonID != DaemonRenameOperationPrefix+"stable-operation" {
		t.Fatalf("DaemonRenameOperationID = %q / %v", daemonID, err)
	}
	observationID, err := ComponentRenameObservationID("native-event")
	if err != nil || observationID != ComponentRenameObservationPrefix+"native-event" {
		t.Fatalf("ComponentRenameObservationID = %q / %v", observationID, err)
	}
	if _, err := DaemonRenameOperationID(observationID); err == nil {
		t.Fatal("daemon rename helper accepted a component namespace collision")
	}
	requests := []struct {
		id    string
		valid bool
	}{
		{DaemonRenameOperationPrefix + "stable-operation", true},
		{ComponentRenameObservationPrefix + "native-event", false},
		{"rename-without-namespace", false},
	}
	for _, candidate := range requests {
		frame, err := NewFrame(TypeSessionRenameRequest, candidate.id, 1, SessionRenameRequest{NativeSessionID: "native", RequestedName: "new name"})
		if err != nil {
			t.Fatal(err)
		}
		if got := ValidatePayload(frame) == nil; got != candidate.valid {
			t.Fatalf("request id %q valid = %t, want %t", candidate.id, got, candidate.valid)
		}
	}
	for _, id := range []string{DaemonRenameOperationPrefix + "response", ComponentRenameObservationPrefix + "observation"} {
		frame, err := NewFrame(TypeSessionRename, id, 1, SessionRename{NativeSessionID: "native", NativeName: "new name", ProductEventSeq: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayload(frame); err != nil {
			t.Fatalf("rename id %q: %v", id, err)
		}
	}
	bad, err := NewFrame(TypeSessionRename, "ambiguous-id", 1, SessionRename{NativeSessionID: "native", NativeName: "new name", ProductEventSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayload(bad); err == nil {
		t.Fatal("session.rename accepted an ambiguous frame-id namespace")
	}
	boundedName, err := NewFrame(TypeSessionRenameRequest, DaemonRenameOperationPrefix+"bounded", 1, SessionRenameRequest{
		NativeSessionID: "native", RequestedName: strings.Repeat("n", 1024),
	})
	if err != nil || ValidatePayload(boundedName) != nil {
		t.Fatalf("maximum bounded rename was rejected: %v", err)
	}
	for _, invalidName := range []string{strings.Repeat("n", 1025), "line\nbreak", " leading"} {
		frame, err := NewFrame(TypeSessionRenameRequest, DaemonRenameOperationPrefix+"invalid-name", 1, SessionRenameRequest{NativeSessionID: "native", RequestedName: invalidName})
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayload(frame); err == nil {
			t.Fatalf("rename request accepted invalid name %q", invalidName)
		}
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
