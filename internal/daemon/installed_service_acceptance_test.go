package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestInstalledServiceContentCanary is activated only by the real installed
// service acceptance. It injects content into accepted and rejected request
// payloads over the production socket; the shell harness then audits every
// service-manager and daemon output sink for the canary.
func TestInstalledServiceContentCanary(t *testing.T) {
	canary := os.Getenv("AGENT_SESSIONS_INSTALLED_CONTENT_CANARY")
	if canary == "" {
		t.Skip("installed service content canary is not requested")
	}
	paths, err := ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := DialControlEndpoint(ctx, paths.ControlEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := writeControlFrame(connection, controlHello{
		Type: "hello", Version: localControlProtocolVersion, RequestID: "acceptance-hello", Role: controlRoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReaderSize(connection, 64*1024)
	body, err := readBoundedControlFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		Type             string `json:"type"`
		DaemonGeneration uint64 `json:"daemon_generation"`
	}
	if err := json.Unmarshal(body, &hello); err != nil || hello.Type != "hello.result" || hello.DaemonGeneration == 0 {
		t.Fatalf("installed daemon hello = %s, err=%v", body, err)
	}
	requests := []struct {
		id        string
		operation string
		accepted  bool
	}{
		{id: "acceptance-accepted", operation: "runtime.status", accepted: true},
		{id: "acceptance-rejected", operation: "daemon.restart", accepted: false},
	}
	for _, request := range requests {
		payload, err := json.Marshal(map[string]any{
			"message": canary + "-message", "prompt": canary + "-prompt",
			"lane_result": canary + "-lane-result", "tool_result": canary + "-tool-result",
			"credential": canary + "-credential", "vendor_transcript": canary + "-transcript",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeControlFrame(connection, controlRequest{
			Type: "request", Version: localControlProtocolVersion, RequestID: request.id,
			Operation: request.operation, ExpectedGeneration: hello.DaemonGeneration, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
		body, err := readBoundedControlFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		var response controlResponse
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if response.RequestID != request.id || response.Accepted != request.accepted {
			t.Fatalf("installed response = %s, want request=%s accepted=%v", body, request.id, request.accepted)
		}
	}
}
