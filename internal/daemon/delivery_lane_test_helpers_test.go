package daemon

import (
	"context"
	"testing"
)

func attachDeliveryLaneTestParticipant(
	t *testing.T,
	registry *AttachmentRegistry,
	product, name, sessionID, cwd string,
	groups []string,
	pid int,
) AttachmentRecord {
	t.Helper()
	nativeActor := map[string]any{"pid": pid, "proc_start": "stable-" + sessionID}
	prepared, capability, err := registry.Prepare(context.Background(), AttachmentPrepareRequest{
		Product: product, Kind: "interactive", Cwd: cwd, Name: name, Groups: groups,
		ExpectedNativeActor: nativeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := registry.Adopt(context.Background(), AttachmentAdoptRequest{
		AttachmentID: prepared.AttachmentID, Capability: capability, SessionID: sessionID,
		NativeActor: nativeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return attached
}
