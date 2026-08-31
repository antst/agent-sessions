package daemon

import (
	"context"
	"testing"
)

func TestQwenLaneAdapterIsSoleACPClientAndPreservesResumeAndYoloMode(t *testing.T) {
	adapter := NewQwenLaneAdapter()
	started := make(chan QwenACPLaneRequest, 1)
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Run(context.Background(), QwenACPLaneRequest{
			LaneID: "lane", NativeSession: "native", Prompt: "one", PermissionMode: "yolo",
		}, func(ctx context.Context, request QwenACPLaneRequest) (NativeACPLaneResult, error) {
			started <- request
			<-ctx.Done()
			return NativeACPLaneResult{NativeSessionID: request.NativeSession}, ctx.Err()
		})
		done <- err
	}()
	request := <-started
	if request.PermissionMode != "bypassPermissions" || request.NativeSession != "native" {
		t.Fatalf("qwen request = %+v", request)
	}
	if _, err := adapter.Run(context.Background(), QwenACPLaneRequest{LaneID: "lane", Prompt: "two"}, func(context.Context, QwenACPLaneRequest) (NativeACPLaneResult, error) {
		return NativeACPLaneResult{}, nil
	}); err == nil {
		t.Fatal("second Qwen ACP client was accepted")
	}
	if err := adapter.Interrupt("lane"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("interrupted Qwen ACP client reported success")
	}
	if err := adapter.Archive("lane"); err != nil {
		t.Fatal(err)
	}
}
