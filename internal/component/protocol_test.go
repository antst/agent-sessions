package component

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestLiveFrameVocabularyRoundTrips(t *testing.T) {
	frames := []struct {
		typeName FrameType
		id       string
		payload  any
	}{
		{TypeSessionAnnounce, "announce", SessionAnnounce{BindingID: "connection", NativeSessionID: "native", Cwd: "/work", NativeName: "title", ProductEventSeq: 1}},
		{TypeSessionRebind, "rebind", SessionRebind{BindingID: "connection", OldNativeSessionID: "old", NewNativeSessionID: "new", Evidence: json.RawMessage(`{}`), ProductEventSeq: 2}},
		{TypeSessionRename, "component.rename.title", SessionRename{NativeSessionID: "native", NativeName: "title", ProductEventSeq: 3}},
		{TypeSessionRenameRequest, "daemon.rename.title", SessionRenameRequest{NativeSessionID: "native", RequestedName: "title"}},
		{TypeSessionState, "state", SessionState{NativeSessionID: "native", State: "idle", ProductEventSeq: 4}},
		{TypeSessionClose, "close", SessionClose{NativeSessionID: "native", Reason: "closed"}},
		{TypeSessionBound, "bound", SessionBound{BindingID: "connection", AttachmentID: "attachment", NativeSessionID: "native", PublicName: "name"}},
		{TypeDeliveryPresent, "delivery", DeliveryPresent{DeliveryID: "delivery", Mode: "idle-wake", Body: json.RawMessage(`{}`)}},
		{TypeDeliveryAccept, "delivery", DeliveryAccept{DeliveryID: "delivery", NativeSessionID: "native", NativeMessageID: "message", AcceptedAt: 1}},
		{TypeDeliveryReject, "delivery", DeliveryReject{DeliveryID: "delivery", Category: CategoryProtocol}},
		{TypeTurnEvent, "turn", TurnEvent{NativeSessionID: "native", EventSeq: 1, Kind: "settled", Metadata: json.RawMessage(`{}`)}},
		{TypeToolCall, "call", ToolCall{CallID: "call", Operation: "peers.list", Arguments: json.RawMessage(`{}`)}},
		{TypeToolCancel, "call", ToolCancel{CallID: "call"}},
		{TypeToolResult, "call", ToolResult{CallID: "call", Success: true, Result: json.RawMessage(`{}`)}},
		{TypeReject, "rejected", Reject{OperationID: "rejected", Category: CategoryProtocol}},
	}
	if len(frames) != len(knownFrameTypes) {
		t.Fatalf("live vocabulary = %d, want %d", len(knownFrameTypes), len(frames))
	}
	for _, test := range frames {
		t.Run(string(test.typeName), func(t *testing.T) {
			frame, err := NewFrame(test.typeName, test.id, 1, test.payload)
			if err != nil {
				t.Fatalf("NewFrame: %v", err)
			}
			body, err := EncodeFrame(frame)
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			decoded, err := DecodeFrame(body)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if decoded.Type != test.typeName || decoded.ID != test.id {
				t.Fatalf("decoded = %#v", decoded)
			}
			if err := ValidatePayload(decoded); err != nil {
				t.Fatalf("ValidatePayload: %v", err)
			}
		})
	}
}

func TestDeletedReliabilityFramesStayDeleted(t *testing.T) {
	for _, frameType := range []FrameType{"bootstrap", "ready", "reconnect", "generation.retire", "heartbeat", "heartbeat.ack"} {
		frame := Frame{Version: ProtocolVersion, Type: frameType, ID: "old", Payload: json.RawMessage(`{}`)}
		if _, err := EncodeFrame(frame); err == nil {
			t.Fatalf("EncodeFrame accepted deleted frame %q", frameType)
		} else {
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Category != CategoryUnknownType {
				t.Fatalf("deleted frame error = %v", err)
			}
		}
	}
}

func TestNativeTitlesKeepProductOwnedEmptyValue(t *testing.T) {
	for _, title := range []string{"", " ", "product title", strings.Repeat("é", 512)} {
		if !ValidNativeTitleObservation(title) {
			t.Fatalf("valid title rejected: %q", title)
		}
		if _, err := NewFrame(TypeSessionAnnounce, "announce", 1, SessionAnnounce{
			BindingID: "connection", NativeSessionID: "native", Cwd: "/work", NativeName: title, ProductEventSeq: 1,
		}); err != nil {
			t.Fatalf("announce title %q: %v", title, err)
		}
	}
	for _, title := range []string{strings.Repeat("x", MaxNativeTitleBytes+1), "bad\x00title", "bad\ntitle"} {
		if ValidNativeTitleObservation(title) {
			t.Fatalf("invalid title accepted: %q", title)
		}
	}
	for _, title := range []string{"", " leading", "trailing "} {
		if _, err := NewFrame(TypeSessionRenameRequest, "daemon.rename.title", 1, SessionRenameRequest{
			NativeSessionID: "native", RequestedName: title,
		}); err == nil {
			t.Fatalf("rename request accepted %q", title)
		}
	}
}

func TestContractRevisionAndRenameNamespaces(t *testing.T) {
	if err := ValidateContractRevision(ContractRevision); err != nil {
		t.Fatal(err)
	}
	if err := ValidateContractRevision("old"); err == nil {
		t.Fatal("old contract revision accepted")
	}
	daemonID, err := DaemonRenameOperationID("stable")
	if err != nil || daemonID != "daemon.rename.stable" {
		t.Fatalf("daemon rename id = %q, %v", daemonID, err)
	}
	componentID, err := ComponentRenameObservationID("native")
	if err != nil || componentID != "component.rename.native" {
		t.Fatalf("component rename id = %q, %v", componentID, err)
	}
}

func TestRedactRemovesSensitiveAssignmentsAndBoundsDetail(t *testing.T) {
	secret := "raw-secret"
	detail := Redact("api_secret="+secret+" password=hunter2 "+strings.Repeat("x", 1024), secret)
	if strings.Contains(detail, secret) || strings.Contains(detail, "hunter2") {
		t.Fatalf("secret leaked: %s", detail)
	}
	if len([]byte(detail)) > maxDetailBytes {
		t.Fatalf("detail length = %d", len([]byte(detail)))
	}
}
