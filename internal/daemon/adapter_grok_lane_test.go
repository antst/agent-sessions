package daemon

import (
	"context"
	"testing"
)

func TestGrokLaneAdapterIsSoleACPDriverAndForcesHeadlessApproval(t *testing.T) {
	adapter := NewGrokLaneAdapter()
	started := make(chan GrokACPLaneRequest, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Run(context.Background(), GrokACPLaneRequest{LaneID: "lane", Prompt: "one", PermissionMode: "default"}, func(ctx context.Context, request GrokACPLaneRequest) (NativeACPLaneResult, error) {
			started <- request
			select {
			case <-ctx.Done():
				return NativeACPLaneResult{}, ctx.Err()
			case <-release:
				return NativeACPLaneResult{NativeSessionID: "native", Mode: request.PermissionMode}, nil
			}
		})
		done <- err
	}()
	request := <-started
	if request.PermissionMode != "bypassPermissions" {
		t.Fatalf("grok permission = %q", request.PermissionMode)
	}
	if _, err := adapter.Run(context.Background(), GrokACPLaneRequest{LaneID: "lane", Prompt: "two"}, func(context.Context, GrokACPLaneRequest) (NativeACPLaneResult, error) {
		return NativeACPLaneResult{}, nil
	}); err == nil {
		t.Fatal("second Grok ACP driver was accepted")
	}
	if err := adapter.Archive("lane"); err == nil {
		t.Fatal("active Grok ACP driver was archived")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := adapter.Archive("lane"); err != nil {
		t.Fatal(err)
	}
}
