package federation

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func FuzzScanMessages(f *testing.F) {
	f.Add([]byte(`{"type":"hello","version":3,"host_id":"host-a","host_name":"host-a","unknown":1}` + "\n"))
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
		resolved, legacy, err := laneCapabilityForMessage(Message{Product: product, Capabilities: capabilities})
		if err != nil {
			return
		}
		if resolved == "" || productcatalog.ValidateToken(resolved) != nil {
			t.Fatalf("admission emitted invalid capability %q", resolved)
		}
		if legacy && len(capabilities) != 0 {
			t.Fatal("non-empty capability frame was marked legacy")
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

func FuzzProspectiveRosterProjection(f *testing.F) {
	seed := Message{Type: "snapshot", Peers: []Peer{
		{
			ID: "host-a/session", HostID: "host-a", HostName: "host-a", SessionID: "session",
			GlobalID: globalSessionID("host-a", "session"), Name: "session",
			Product: "future-product", Entrypoint: "future-product", InstanceID: "future:session",
			PeerProtocol: GroupProtocolVersion, Groups: []string{PrivateGroup("host-a", "session")},
		},
	}}
	body, _ := json.Marshal(seed)
	f.Add(body, true)
	f.Add(body, false)
	f.Fuzz(func(t *testing.T, body []byte, marked bool) {
		var snapshot Message
		if json.Unmarshal(body, &snapshot) != nil || len(snapshot.Peers) > maxSnapshotPeers {
			return
		}
		peers := make(map[string]Peer, len(snapshot.Peers))
		for _, peer := range snapshot.Peers {
			if validateWirePeer(peer, "host-a") != nil {
				return
			}
			if _, duplicate := peers[peer.ID]; duplicate {
				return
			}
			peers[peer.ID] = peer
		}
		capabilities := []string(nil)
		if marked {
			capabilities = []string{transportFeatureOpaquePeerProducts}
		}
		client := &hubClient{
			hostID: "host-a", hostName: "host-a", ready: true,
			capabilities: capabilities, peers: peers,
		}
		h := &hub{clients: map[string]*hubClient{client.hostID: client}}
		deliveries := h.rosterDeliveriesLocked(nil, nil)
		if err := validateRosterDeliveries(deliveries); err != nil {
			return
		}
		for _, delivery := range deliveries {
			encoded, err := json.Marshal(delivery.message)
			if err != nil || len(encoded) > maxWireBytes || len(delivery.message.Peers) > maxRosterPeers {
				t.Fatalf("admitted invalid roster: bytes=%d peers=%d err=%v", len(encoded), len(delivery.message.Peers), err)
			}
			if !marked {
				for _, peer := range delivery.message.Peers {
					if !legacyV3PeerProduct(peer) {
						t.Fatalf("unmarked roster leaked product %q", peer.Product)
					}
				}
			}
		}
	})
}
