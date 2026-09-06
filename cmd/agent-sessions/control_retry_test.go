package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	daemonpkg "github.com/antst/sessionbus/internal/daemon"
)

func TestCallDaemonWithStableRetryReplaysSameRequestAfterLostResponse(t *testing.T) {
	request := daemonpkg.ControlRequest{
		ID:             "invocation-1",
		Role:           daemonpkg.RoleLauncher,
		Operation:      "lane.command",
		IdempotencyKey: "invocation-1",
		Payload:        json.RawMessage(`{"product":"codex","arguments":["run","work"]}`),
	}
	var attempts []daemonpkg.ControlRequest
	handlerCalls := 0
	cached := daemonpkg.ControlResponse{
		ID:         request.ID,
		Generation: 17,
		OK:         true,
		Payload:    json.RawMessage(`{"lane_id":"lane-1"}`),
	}
	response, err := callDaemonWithStableRetry(
		context.Background(),
		"unused-injected-endpoint",
		request,
		func(_ context.Context, _ string, got daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error) {
			attempts = append(attempts, got)
			if got.Generation == 0 {
				return daemonpkg.ControlResponse{
					ID: got.ID, Generation: 17,
					Error: &daemonpkg.ControlFailure{Code: daemonpkg.ErrorStaleGeneration, Message: "stale"},
				}, nil
			}
			if handlerCalls == 0 {
				handlerCalls++
				// The daemon accepted and cached the mutation, but its response was
				// lost with the connection. The next call is its idempotent replay.
				return daemonpkg.ControlResponse{}, io.ErrUnexpectedEOF
			}
			return cached, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response, cached) {
		t.Fatalf("response = %#v, want %#v", response, cached)
	}
	if handlerCalls != 1 {
		t.Fatalf("native handler calls = %d, want 1", handlerCalls)
	}
	if len(attempts) != maxDaemonControlAttempts {
		t.Fatalf("attempts = %d, want %d", len(attempts), maxDaemonControlAttempts)
	}
	for index, got := range attempts {
		want := request
		want.Generation = got.Generation
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("attempt %d changed stable request fields: %#v", index+1, got)
		}
	}
	if attempts[0].Generation != 0 || attempts[1].Generation != 17 || attempts[2].Generation != 17 {
		t.Fatalf("attempt generations = [%d %d %d], want [0 17 17]",
			attempts[0].Generation, attempts[1].Generation, attempts[2].Generation)
	}
}

func TestCallDaemonWithStableRetryIsBoundedAndContextCancellable(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		calls := 0
		_, err := callDaemonWithStableRetry(
			context.Background(),
			"unused-injected-endpoint",
			daemonpkg.ControlRequest{ID: "bounded", IdempotencyKey: "bounded"},
			func(context.Context, string, daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error) {
				calls++
				return daemonpkg.ControlResponse{}, io.ErrUnexpectedEOF
			},
		)
		if err == nil || !strings.Contains(err.Error(), "daemon is unavailable") {
			t.Fatalf("bounded retry error = %v", err)
		}
		if calls != maxDaemonControlAttempts {
			t.Fatalf("bounded retry calls = %d, want %d", calls, maxDaemonControlAttempts)
		}
	})

	t.Run("stale generation", func(t *testing.T) {
		calls := 0
		response, err := callDaemonWithStableRetry(
			context.Background(),
			"unused-injected-endpoint",
			daemonpkg.ControlRequest{ID: "stale", IdempotencyKey: "stale"},
			func(_ context.Context, _ string, request daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error) {
				calls++
				return daemonpkg.ControlResponse{
					ID: request.ID, Generation: request.Generation + 1,
					Error: &daemonpkg.ControlFailure{Code: daemonpkg.ErrorStaleGeneration, Message: "stale"},
				}, nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if response.Error == nil || response.Error.Code != daemonpkg.ErrorStaleGeneration {
			t.Fatalf("bounded stale response = %#v", response)
		}
		if calls != maxDaemonControlAttempts {
			t.Fatalf("bounded stale calls = %d, want %d", calls, maxDaemonControlAttempts)
		}
	})

	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		_, err := callDaemonWithStableRetry(
			ctx,
			"unused-injected-endpoint",
			daemonpkg.ControlRequest{ID: "cancelled", IdempotencyKey: "cancelled"},
			func(context.Context, string, daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error) {
				calls++
				cancel()
				return daemonpkg.ControlResponse{}, io.ErrUnexpectedEOF
			},
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled retry error = %v, want context canceled", err)
		}
		if calls != 1 {
			t.Fatalf("cancelled retry calls = %d, want 1", calls)
		}
	})
}

func TestCallDaemonWithStableRetryDoesNotRetryDaemonFailure(t *testing.T) {
	calls := 0
	want := daemonpkg.ControlResponse{
		ID:         "semantic-failure",
		Generation: 9,
		Error:      &daemonpkg.ControlFailure{Code: daemonpkg.ErrorHandler, Message: "product rejected request"},
	}
	got, err := callDaemonWithStableRetry(
		context.Background(),
		"unused-injected-endpoint",
		daemonpkg.ControlRequest{ID: "semantic-failure", IdempotencyKey: "semantic-failure"},
		func(context.Context, string, daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error) {
			calls++
			return want, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
	if calls != 1 {
		t.Fatalf("daemon application failure calls = %d, want 1", calls)
	}
}
