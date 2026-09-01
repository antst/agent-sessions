package codebuddy

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestParentAttestationUsesKernelProcessAndPerSessionAncestry(t *testing.T) {
	worker := procinfo.Identity{PID: 100, Start: "worker", StrongStart: "worker-strong"}
	connector := procinfo.Identity{PID: 101, Start: "connector", StrongStart: "connector-strong"}
	processes := &fakeProcesses{
		identities: map[int]procinfo.Identity{worker.PID: worker, connector.PID: connector},
		descends:   map[[2]int]bool{{connector.PID, worker.PID}: true},
	}
	attachments := ActiveAttachmentSourceFunc(func(context.Context) ([]daemon.ManagedAttachment, error) {
		return []daemon.ManagedAttachment{{
			ID: "attachment", Product: ProductID, NativeSessionID: "native", State: "attached",
			Evidence: daemon.NativeEvidence{Process: worker},
		}}, nil
	})
	attester, err := NewParentAttester(processes, attachments, 16)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := attester.Attest(context.Background(), productruntime.ConnectorAttempt{
		ProductID: ProductID, ProcessIdentity: connector, ClaimedNativeSessionID: "native",
	})
	if err != nil || !binding.Verified || binding.AttachmentID != "attachment" || binding.NativeSessionID != "native" {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	if _, err := attester.Attest(context.Background(), productruntime.ConnectorAttempt{
		ProductID: ProductID, ProcessIdentity: connector, ClaimedNativeSessionID: "forged",
	}); !errors.Is(err, productruntime.ErrUnauthorized) {
		t.Fatalf("forged claim error = %v", err)
	}
	if _, err := attester.Attest(context.Background(), productruntime.ConnectorAttempt{
		ProductID: ProductID, ProcessIdentity: connector, ComponentBindingID: "sidecar-binding",
	}); !errors.Is(err, productruntime.ErrUnauthorized) {
		t.Fatalf("component-sidecar error = %v", err)
	}
}

func TestParentAttestationRejectsAmbiguousCrossTargetAncestry(t *testing.T) {
	workerA := procinfo.Identity{PID: 100, Start: "a", StrongStart: "as"}
	workerB := procinfo.Identity{PID: 200, Start: "b", StrongStart: "bs"}
	connector := procinfo.Identity{PID: 300, Start: "c", StrongStart: "cs"}
	processes := &fakeProcesses{
		identities: map[int]procinfo.Identity{100: workerA, 200: workerB, 300: connector},
		descends:   map[[2]int]bool{{300, 100}: true, {300, 200}: true},
	}
	attester, _ := NewParentAttester(processes, ActiveAttachmentSourceFunc(func(context.Context) ([]daemon.ManagedAttachment, error) {
		return []daemon.ManagedAttachment{
			{ID: "a", Product: ProductID, NativeSessionID: "a", State: "attached", Evidence: daemon.NativeEvidence{Process: workerA}},
			{ID: "b", Product: ProductID, NativeSessionID: "b", State: "attached", Evidence: daemon.NativeEvidence{Process: workerB}},
		}, nil
	}), 16)
	if _, err := attester.Attest(context.Background(), productruntime.ConnectorAttempt{
		ProductID: ProductID, ProcessIdentity: connector,
	}); !errors.Is(err, productruntime.ErrUnauthorized) {
		t.Fatalf("ambiguous ancestry error = %v", err)
	}
}
