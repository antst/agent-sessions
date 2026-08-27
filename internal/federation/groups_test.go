package federation

import (
	"encoding/json"
	"testing"
)

func TestSessionPreferenceContractRoundTripsProductNeutralGroups(t *testing.T) {
	want := SessionPreferences{
		SessionID: "session-a", Product: "qwen", Kind: SessionKindLane,
		ExplicitGroups: []string{"project"}, InheritedGroups: []string{"parent"},
		ParentSession: "parent-a", ParentHostID: "host-a", InheritParentGroups: true,
		Qwen: &QwenSessionMetadata{Cwd: "/workspace", Profile: QwenProfileIdentity{Fingerprint: "profile"}},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got SessionPreferences
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != want.SessionID || got.Kind != SessionKindLane || got.Qwen == nil || got.Qwen.Profile.Fingerprint != "profile" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestSessionKindsAreClosed(t *testing.T) {
	if SessionKindInteractive != "interactive" || SessionKindLane != "lane" {
		t.Fatalf("session kinds = %q, %q", SessionKindInteractive, SessionKindLane)
	}
}
