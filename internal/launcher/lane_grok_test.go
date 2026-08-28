package launcher

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
)

func TestGrokLaneLifecycleCommandsUseDaemonWithoutExecutableResolution(t *testing.T) {
	previousQuery, previousInput, previousOutput := queryLaneDaemon, laneInput, laneOutput
	t.Cleanup(func() {
		queryLaneDaemon, laneInput, laneOutput = previousQuery, previousInput, previousOutput
	})
	queries := 0
	queryLaneDaemon = func(_ context.Context, _ daemon.LocalControlIdentity, operation string, payload any) (daemon.LocalControlResult, error) {
		queries++
		request, ok := payload.(daemon.LaneCommandRequest)
		if !ok || operation != "lane.command" || request.Product != "grok" {
			t.Fatalf("daemon lane query = %q %#v", operation, payload)
		}
		return daemon.LocalControlResult{Result: json.RawMessage(`{"type":"lane.result"}`)}, nil
	}
	laneOutput = discardWriter{}

	for _, command := range []string{"wait", "status", "interrupt", "archive", "list", "doctor"} {
		arguments := []string{command}
		if command != "list" && command != "doctor" {
			arguments = append(arguments, "lane-a")
		}
		if err := RunLane("grok-lane", arguments); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	if queries != 6 {
		t.Fatalf("daemon queries = %d, want 6", queries)
	}
}

type discardWriter struct{}

func (discardWriter) Write(body []byte) (int, error) { return len(body), nil }
