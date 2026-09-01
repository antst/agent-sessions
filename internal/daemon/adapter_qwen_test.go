package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestQwenAdapterAdoptsExactDualOutputEvidence(t *testing.T) {
	owner := startGrokAdapterIdentity(t)
	evidence := NativeEvidence{
		Process: owner, ThreadID: "qwen-session", RegistryPath: "/tmp/qwen-event.json",
		ArtifactPath: "/tmp/qwen-input.json", ArtifactRevision: "revision-1",
	}
	attachment := ManagedAttachment{
		ID: "attachment-qwen", Product: "qwen", NativeSessionID: "qwen-session",
		ExpectedEvidence: evidence,
	}
	adopt := NewQwenAttachmentAdapter(QwenAdapterConfig{}).Adopt
	if _, err := adopt(context.Background(), attachment, evidence); err != nil {
		t.Fatalf("adopt exact Qwen evidence: %v", err)
	}
	for name, mutate := range map[string]func(*NativeEvidence){
		"event-output": func(value *NativeEvidence) { value.RegistryPath = "/tmp/other-event.json" },
		"input-output": func(value *NativeEvidence) { value.ArtifactPath = "/tmp/other-input.json" },
		"revision":     func(value *NativeEvidence) { value.ArtifactRevision = "" },
		"session":      func(value *NativeEvidence) { value.ThreadID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := evidence
			mutate(&changed)
			if _, err := adopt(context.Background(), attachment, changed); err == nil {
				t.Fatal("Qwen adoption accepted changed dual-output evidence")
			}
		})
	}
}

func TestQwenAdapterAuthorizesOnlyNativeTUIOrDescendant(t *testing.T) {
	owner := startGrokAdapterIdentity(t)
	caller, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	attachment := ManagedAttachment{
		ID: "attachment-qwen", Product: "qwen", NativeSessionID: "qwen-session",
		Evidence: NativeEvidence{Process: owner, ThreadID: "qwen-session"},
	}
	authorize := NewQwenAttachmentAdapter(QwenAdapterConfig{}).Authorize
	if err := authorize(context.Background(), attachment, NativeEvidence{
		Process: caller, Ancestry: []procinfo.Identity{owner}, ThreadID: "qwen-session",
	}); err != nil {
		t.Fatalf("authorize Qwen descendant: %v", err)
	}
	for name, observed := range map[string]NativeEvidence{
		"unrelated": {Process: caller, Ancestry: []procinfo.Identity{caller}, ThreadID: "qwen-session"},
		"session":   {Process: caller, Ancestry: []procinfo.Identity{owner}, ThreadID: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := authorize(context.Background(), attachment, observed); err == nil {
				t.Fatal("inexact Qwen actor was authorized")
			}
		})
	}
}
