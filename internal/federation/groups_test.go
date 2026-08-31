package federation

import "testing"

func TestChildGroupsAlwaysAnchorImmediateParentAndOptionallyInherit(t *testing.T) {
	parent, err := EffectiveGroups("host-a", "parent", []string{"team"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := ChildGroups("host-a", "child", "host-a", "parent", nil, parent, false)
	if err != nil {
		t.Fatal(err)
	}
	wantIsolated := map[string]bool{PrivateGroup("host-a", "parent"): true, PrivateGroup("host-a", "child"): true}
	if len(isolated) != 2 || !wantIsolated[isolated[0]] || !wantIsolated[isolated[1]] {
		t.Fatalf("isolated child groups = %v", isolated)
	}
	inherited, err := ChildGroups("host-a", "child", "host-a", "parent", []string{"child-team"}, parent, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"team", "child-team", PrivateGroup("host-a", "parent"), PrivateGroup("host-a", "child")} {
		if !contains(inherited, want) {
			t.Fatalf("inherited groups %v omit %q", inherited, want)
		}
	}
}

func TestAdmitFiltersHiddenCollisionsAndSnapshotsMulticastAndBroadcast(t *testing.T) {
	source := Peer{ID: "source", SessionID: "source", Name: "source", Groups: []string{"team"}}
	visibleA := Peer{ID: "a", SessionID: "a", Name: "duplicate", Groups: []string{"team"}}
	visibleB := Peer{ID: "b", SessionID: "b", Name: "reader", Groups: []string{"team", "other"}}
	hidden := Peer{ID: "hidden", SessionID: "hidden", Name: "duplicate", Groups: []string{"secret"}}
	admission, err := Admit(AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "m1", Targets: []string{"duplicate", "b"}, Content: "hello",
	}, source, []Peer{hidden, visibleB, visibleA})
	if err != nil {
		t.Fatal(err)
	}
	if len(admission.Targets) != 2 || admission.Targets[0].ID != "a" || admission.Targets[1].ID != "b" {
		t.Fatalf("multicast targets = %+v", admission.Targets)
	}
	broadcast, err := Admit(AgentFrame{
		Version: AgentFrameVersion, Type: "broadcast", MessageID: "m2", Group: "team", Content: "hello",
	}, source, []Peer{hidden, visibleB, visibleA})
	if err != nil {
		t.Fatal(err)
	}
	if len(broadcast.Targets) != 2 || broadcast.Targets[0].ID != "a" || broadcast.Targets[1].ID != "b" {
		t.Fatalf("broadcast targets = %+v", broadcast.Targets)
	}
}

func TestAdmitRejectsDuplicateResolutionAndUnauthorizedBroadcast(t *testing.T) {
	source := Peer{ID: "source", SessionID: "source", Groups: []string{"team"}}
	peer := Peer{ID: "a", SessionID: "session-a", Name: "reader", Groups: []string{"team"}}
	if _, err := Admit(AgentFrame{
		Version: AgentFrameVersion, Type: "send", MessageID: "m1", Targets: []string{"a", "reader"}, Content: "hello",
	}, source, []Peer{peer}); err == nil {
		t.Fatal("send accepted two aliases resolving to one destination")
	}
	if _, err := Admit(AgentFrame{
		Version: AgentFrameVersion, Type: "broadcast", MessageID: "m2", Group: "secret", Content: "hello",
	}, source, []Peer{peer}); err == nil {
		t.Fatal("broadcast accepted a nonmember sender")
	}
}
