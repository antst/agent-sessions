package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/antst/sessionbus/internal/procinfo"
)

func TestCodexAdapterAdoptsExactThreadOwnerAppServerAndSocket(t *testing.T) {
	owner := startGrokAdapterIdentity(t)
	appServer := startGrokAdapterIdentity(t)
	evidence := NativeEvidence{
		Process: owner, Ancestry: []procinfo.Identity{appServer},
		ThreadID: "thread-codex", SocketPath: "/tmp/codex-app-server.sock",
	}
	attachment := ManagedAttachment{
		ID: "thread-codex", Product: "codex", NativeSessionID: "thread-codex",
		ExpectedEvidence: evidence,
	}
	adopt := NewCodexAttachmentAdapter(CodexAdapterConfig{}).Adopt
	if _, err := adopt(context.Background(), attachment, evidence); err != nil {
		t.Fatalf("adopt exact Codex evidence: %v", err)
	}
	changed := evidence
	changed.SocketPath = "/tmp/other.sock"
	if _, err := adopt(context.Background(), attachment, changed); err == nil {
		t.Fatal("Codex adoption accepted a changed App Server socket")
	}
	changed = evidence
	changed.Ancestry = []procinfo.Identity{startGrokAdapterIdentity(t)}
	if _, err := adopt(context.Background(), attachment, changed); err == nil {
		t.Fatal("Codex adoption accepted a changed App Server identity")
	}
}

func TestCodexAdapterAuthorizesOnlyExactManagedThreadAncestry(t *testing.T) {
	owner := startGrokAdapterIdentity(t)
	appServer := startGrokAdapterIdentity(t)
	caller, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	attachment := ManagedAttachment{
		ID: "thread-codex", Product: "codex", NativeSessionID: "thread-codex",
		Evidence: NativeEvidence{
			Process: owner, Ancestry: []procinfo.Identity{appServer},
			ThreadID: "thread-codex", SocketPath: "/tmp/codex-app-server.sock",
		},
	}
	authorize := NewCodexAttachmentAdapter(CodexAdapterConfig{}).Authorize
	for _, ancestry := range [][]procinfo.Identity{{owner}, {appServer}, {caller, owner}} {
		if err := authorize(context.Background(), attachment, NativeEvidence{
			Process: caller, Ancestry: ancestry, ThreadID: "thread-codex", SocketPath: "/tmp/codex-app-server.sock",
		}); err != nil {
			t.Fatalf("authorize exact Codex ancestry %+v: %v", ancestry, err)
		}
	}
	requireAuthorizationRejected(t, authorize, attachment, map[string]NativeEvidence{
		"wrong-thread": {Process: caller, Ancestry: []procinfo.Identity{owner}, ThreadID: "other", SocketPath: "/tmp/codex-app-server.sock"},
		"wrong-socket": {Process: caller, Ancestry: []procinfo.Identity{owner}, ThreadID: "thread-codex", SocketPath: "/tmp/other.sock"},
		"wrong-actor":  {Process: caller, Ancestry: []procinfo.Identity{caller}, ThreadID: "thread-codex", SocketPath: "/tmp/codex-app-server.sock"},
	})
}
