package federation

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func FuzzScanMessages(f *testing.F) {
	f.Add([]byte(`{"type":"hello","version":4,"host_id":"host-a","host_name":"host-a","unknown":1}` + "\n"))
	f.Add([]byte("{not-json}\n"))
	f.Add(bytes.Repeat([]byte("x"), maxWireBytes+1))
	f.Fuzz(func(t *testing.T, body []byte) {
		_ = scanMessages(bytes.NewReader(body), func(message Message) error {
			_, _ = json.Marshal(message)
			return nil
		})
	})
}

func FuzzNormalizeCapabilities(f *testing.F) {
	f.Add("future-lane", "codex-lane")
	f.Add("", "Future")
	f.Fuzz(func(t *testing.T, first, second string) {
		values, err := normalizeCapabilities([]string{first, second, first})
		if err != nil {
			return
		}
		if len(values) > 2 {
			t.Fatalf("deduplication failed: %q", values)
		}
		for index, value := range values {
			if err := productcatalog.ValidateToken(value); err != nil {
				t.Fatalf("normalizer emitted invalid token %q: %v", value, err)
			}
			if index > 0 && values[index-1] >= value {
				t.Fatalf("normalizer output is not sorted and unique: %q", values)
			}
		}
	})
}

func FuzzLaneCapabilityAdmission(f *testing.F) {
	f.Add("future", "future-lane", uint8(1))
	f.Add("codex", "", uint8(0))
	f.Fuzz(func(t *testing.T, product, capability string, count uint8) {
		capabilities := make([]string, int(count%4))
		for index := range capabilities {
			capabilities[index] = capability
		}
		resolved, err := laneCapabilityForMessage(Message{Product: product, Capabilities: capabilities})
		if err != nil {
			return
		}
		if resolved == "" || productcatalog.ValidateToken(resolved) != nil {
			t.Fatalf("admission emitted invalid capability %q", resolved)
		}
		if len(capabilities) != 1 || resolved != capabilities[0] {
			t.Fatalf("admission changed explicit capability %q to %q", capabilities, resolved)
		}
	})
}

func FuzzWirePeerValidation(f *testing.F) {
	seed := Peer{
		ID: "host-a/session", HostID: "host-a", SessionID: "session",
		GlobalID: globalSessionID("host-a", "session"), Name: "session",
		Product: "future-product", Entrypoint: "future-product", InstanceID: "instance",
		PeerProtocol: GroupProtocolVersion, Groups: []string{PrivateGroup("host-a", "session")},
	}
	body, _ := json.Marshal(seed)
	f.Add(body)
	f.Fuzz(func(t *testing.T, body []byte) {
		var peer Peer
		if json.Unmarshal(body, &peer) != nil {
			return
		}
		_ = validateWirePeer(peer, peer.HostID)
	})
}
