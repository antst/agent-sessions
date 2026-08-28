package launcher

import (
	"testing"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestLauncherProductProjectionCoversEveryDescriptor(t *testing.T) {
	for _, descriptor := range productcatalog.ProductDescriptors() {
		product, ok := launcherProductByID(descriptor.ID)
		if !ok {
			t.Fatalf("launcher projection is missing %s", descriptor.ID)
		}
		for _, kind := range []string{federation.SessionKindInteractive, federation.SessionKindLane} {
			executable, arguments, resumeOK := product.resume(kind, "session-a")
			if !resumeOK || len(arguments) != 2 {
				t.Fatalf("%s %s resume projection = %q %v, %v", descriptor.ID, kind, executable, arguments, resumeOK)
			}
			wantExecutable := descriptor.PeerAlias
			if kind == federation.SessionKindLane {
				wantExecutable = descriptor.LaneAlias
			}
			if executable != wantExecutable {
				t.Fatalf("%s %s executable = %q, want %q", descriptor.ID, kind, executable, wantExecutable)
			}
		}
		if projected, ok := launcherProductByLaneRole(descriptor.LaneRuntimeRole); !ok || projected.descriptor.ID != descriptor.ID {
			t.Fatalf("lane role %q did not resolve to %s", descriptor.LaneRuntimeRole, descriptor.ID)
		}
	}
}
