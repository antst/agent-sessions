package localtransport

import "testing"

func TestPeerIdentityIsTypeOnlyAndFailClosed(t *testing.T) {
	if !(PeerIdentity{PID: 22, UID: 1000}).Valid() {
		t.Fatal("corroborated peer identity rejected")
	}
	for _, identity := range []PeerIdentity{{}, {PID: 1, UID: 1000}, {PID: 22, UID: -1}} {
		if identity.Valid() {
			t.Fatalf("invalid identity accepted: %#v", identity)
		}
	}
}
