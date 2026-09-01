package codebuddy

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestNewRuntimeExportsCompletePinnedExperimentalDriverSet(t *testing.T) {
	config := codebuddyTestConfig(t, &fakeRegistry{}, &fakeProcesses{})
	config.Attachments = ActiveAttachmentSourceFunc(func(context.Context) ([]daemon.ManagedAttachment, error) { return nil, nil })
	config.Deps = productruntime.HostDeps{
		Generation: 1, OwnedProcesses: newFakeOwnedSupervisor(),
		Receipts: memoryReceiptReader{values: map[string][]byte{"receipt": []byte("body")}},
	}
	descriptor := productcatalog.Descriptor{
		ID: ProductID, TestedVersion: PinnedVersion, SupportState: productcatalog.SupportExperimental,
		Compatibility: productcatalog.Compatibility{Policy: productcatalog.VersionExact},
	}
	runtime, err := NewRuntime(descriptor, config)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Peer == nil || runtime.Message == nil || runtime.Lane == nil || runtime.Parent == nil || runtime.Doctor == nil {
		t.Fatalf("incomplete CodeBuddy runtime = %#v", runtime)
	}
	descriptor.SupportState = productcatalog.SupportGeneral
	if _, err := NewRuntime(descriptor, config); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("general-support constructor error = %v", err)
	}
}
