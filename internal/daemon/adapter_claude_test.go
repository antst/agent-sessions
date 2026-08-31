package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestClaudeAdapterAdoptsPresenceSensitivePIDRowAndSocket(t *testing.T) {
	owner := startGrokAdapterIdentity(t)
	evidence := NativeEvidence{
		Process: owner, ThreadID: "claude-session", RegistryPath: "/tmp/claude-session.json",
		SocketPath: "/tmp/claude-session.sock",
	}
	attachment := ManagedAttachment{
		ID: "attachment-claude", Product: "claude", NativeSessionID: "claude-session",
		ExpectedEvidence: evidence,
	}
	adopt := NewClaudeAttachmentAdapter(ClaudeAdapterConfig{}).Adopt
	if _, err := adopt(context.Background(), attachment, evidence); err != nil {
		t.Fatalf("adopt exact Claude evidence: %v", err)
	}
	for name, mutate := range map[string]func(*NativeEvidence){
		"pid-row": func(value *NativeEvidence) { value.RegistryPath = "" },
		"socket":  func(value *NativeEvidence) { value.SocketPath = "/tmp/other.sock" },
		"session": func(value *NativeEvidence) { value.ThreadID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := evidence
			mutate(&changed)
			if _, err := adopt(context.Background(), attachment, changed); err == nil {
				t.Fatal("Claude adoption accepted changed native evidence")
			}
		})
	}
}

func TestClaudeAdapterAuthorizesOnlyNativeTUIOrDescendant(t *testing.T) {
	owner := startGrokAdapterIdentity(t)
	caller, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	attachment := ManagedAttachment{
		ID: "attachment-claude", Product: "claude", NativeSessionID: "claude-session",
		Evidence: NativeEvidence{Process: owner, ThreadID: "claude-session", SocketPath: "/tmp/claude-session.sock"},
	}
	authorize := NewClaudeAttachmentAdapter(ClaudeAdapterConfig{}).Authorize
	if err := authorize(context.Background(), attachment, NativeEvidence{
		Process: caller, Ancestry: []procinfo.Identity{owner}, ThreadID: "claude-session", SocketPath: "/tmp/claude-session.sock",
	}); err != nil {
		t.Fatalf("authorize Claude descendant: %v", err)
	}
	requireAuthorizationRejected(t, authorize, attachment, map[string]NativeEvidence{
		"unrelated": {Process: caller, Ancestry: []procinfo.Identity{caller}, ThreadID: "claude-session", SocketPath: "/tmp/claude-session.sock"},
		"session":   {Process: caller, Ancestry: []procinfo.Identity{owner}, ThreadID: "other", SocketPath: "/tmp/claude-session.sock"},
		"socket":    {Process: caller, Ancestry: []procinfo.Identity{owner}, ThreadID: "claude-session", SocketPath: "/tmp/other.sock"},
	})
}
