package daemon

import (
	"context"
	"errors"
	"testing"
)

func TestGrokLaneAdapterRejectsUnsupportedPolicyBeforeNativeCallback(t *testing.T) {
	adapter := NewGrokLaneAdapter()
	for _, mode := range []string{"", "default", "acceptEdits", " bypassPermissions", "unknown"} {
		called := false
		request := GrokACPLaneRequest{LaneID: "lane", Prompt: "one", PermissionMode: mode}
		_, err := adapter.Run(context.Background(), request, func(context.Context, GrokACPLaneRequest) (NativeACPLaneResult, error) {
			called = true
			return NativeACPLaneResult{}, nil
		})
		if !errors.Is(err, ErrGrokUnsupportedPermissionMode) || called {
			t.Fatalf("mode %q = %v, called=%v", mode, err, called)
		}
		if request.PermissionMode != mode {
			t.Fatalf("mode %q mutated to %q", mode, request.PermissionMode)
		}
	}
}

func TestGrokLaneAdapterIsSoleACPDriverAndPreservesExplicitPolicy(t *testing.T) {
	adapter := NewGrokLaneAdapter()
	started := make(chan GrokACPLaneRequest, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Run(context.Background(), GrokACPLaneRequest{LaneID: "lane", Prompt: "one", PermissionMode: "bypassPermissions"}, func(ctx context.Context, request GrokACPLaneRequest) (NativeACPLaneResult, error) {
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
	if _, err := adapter.Run(context.Background(), GrokACPLaneRequest{LaneID: "lane", Prompt: "two", PermissionMode: "bypassPermissions"}, func(context.Context, GrokACPLaneRequest) (NativeACPLaneResult, error) {
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

func TestGrokLaneAdapterResumeIdentityInterruptAndArchiveAreUnchanged(t *testing.T) {
	adapter := NewGrokLaneAdapter()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Run(context.Background(), GrokACPLaneRequest{
			LaneID: "resume", NativeSession: "native-one", Prompt: "continue", PermissionMode: "bypassPermissions",
		}, func(ctx context.Context, request GrokACPLaneRequest) (NativeACPLaneResult, error) {
			close(started)
			<-ctx.Done()
			return NativeACPLaneResult{}, ctx.Err()
		})
		done <- err
	}()
	<-started
	if err := adapter.Interrupt("resume"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt result = %v", err)
	}
	if err := adapter.Archive("resume"); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Run(context.Background(), GrokACPLaneRequest{
		LaneID: "mismatch", NativeSession: "native-one", Prompt: "continue", PermissionMode: "bypassPermissions",
	}, func(context.Context, GrokACPLaneRequest) (NativeACPLaneResult, error) {
		return NativeACPLaneResult{NativeSessionID: "native-two"}, nil
	})
	if err == nil || result.NativeSessionID != "" {
		t.Fatalf("resume mismatch = %#v, %v", result, err)
	}
}
