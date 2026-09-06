package daemon

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/antst/sessionbus/internal/procinfo"
)

func TestGrokAdapterAuthorizesExactTUIOrPrivateLeaderConnector(t *testing.T) {
	owner := startGrokAdapterIdentity(t)
	leader := startGrokAdapterIdentity(t)
	caller, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	attachment := ManagedAttachment{
		Product: "grok", NativeSessionID: "native-grok",
		Evidence: NativeEvidence{Process: owner, Ancestry: []procinfo.Identity{leader}},
	}
	authorize := NewGrokAttachmentAdapter(GrokAdapterConfig{}).Authorize
	for _, ancestry := range [][]procinfo.Identity{{owner}, {leader}, {caller, owner}, {caller, leader}} {
		err := authorize(context.Background(), attachment, NativeEvidence{
			Process: caller, Ancestry: ancestry, ThreadID: "native-grok",
		})
		if err != nil {
			t.Fatalf("authorize Grok ancestry %+v: %v", ancestry, err)
		}
	}
	if err := authorize(context.Background(), attachment, NativeEvidence{
		Process: caller, Ancestry: []procinfo.Identity{caller}, ThreadID: "native-grok",
	}); err == nil {
		t.Fatal("unrelated Grok ancestry was authorized")
	}
}

func startGrokAdapterIdentity(t *testing.T) procinfo.Identity {
	t.Helper()
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	identity, err := procinfo.CaptureIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
