package procinfo

import (
	"os"
	"os/exec"
	"testing"
)

func TestExactIdentityClassifiesPIDStartStrongStartAndStateWithoutCollapsingUnknown(t *testing.T) {
	expected := Identity{PID: 42, Start: "coarse", StrongStart: "strong"}
	tests := []struct {
		name string
		info Info
		want IdentityStatus
	}{
		{name: "unknown", info: Info{Status: Unknown}, want: IdentityUnknown},
		{name: "absent", info: Info{Status: Absent}, want: IdentityStale},
		{name: "zombie", info: Info{Status: Known, State: "Z", Start: "coarse", StrongStart: "strong"}, want: IdentityStale},
		{name: "missing coarse", info: Info{Status: Known, State: "S", StrongStart: "strong"}, want: IdentityUnknown},
		{name: "coarse reused", info: Info{Status: Known, State: "S", Start: "other", StrongStart: "strong"}, want: IdentityStale},
		{name: "missing strong", info: Info{Status: Known, State: "S", Start: "coarse"}, want: IdentityUnknown},
		{name: "strong reused", info: Info{Status: Known, State: "S", Start: "coarse", StrongStart: "other"}, want: IdentityStale},
		{name: "exact", info: Info{Status: Known, State: "S", Start: "coarse", StrongStart: "strong"}, want: IdentityMatches},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation := ClassifyIdentity(expected, test.info)
			if observation.Status != test.want {
				t.Fatalf("identity observation = %+v, want status %v", observation, test.want)
			}
		})
	}
	if got := ClassifyIdentity(Identity{PID: 42, Start: "coarse"}, Info{Status: Known, State: "S", Start: "coarse", StrongStart: "current"}); got.Status != IdentityMatches {
		t.Fatalf("coarse baseline identity compatibility = %+v", got)
	}
}

func TestCaptureObserveAndAncestryUseExactHostIdentity(t *testing.T) {
	parent, err := CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if parent.PID != os.Getpid() || parent.Start == "" || parent.StrongStart == "" {
		t.Fatalf("captured parent = %+v", parent)
	}
	if observation := ObserveIdentity(parent); observation.Status != IdentityMatches || observation.Current != parent {
		t.Fatalf("observed parent = %+v", observation)
	}

	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	child, err := CaptureIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := DescendsFrom(child, parent, 8)
	if err != nil || !owned {
		t.Fatalf("child ancestry = %v, %v; child=%+v parent=%+v", owned, err, child, parent)
	}
	reused := parent
	reused.StrongStart += "-reused"
	if owned, err := DescendsFrom(child, reused, 8); err != nil || owned {
		t.Fatalf("reused ancestor was accepted: %v, %v", owned, err)
	}
}
