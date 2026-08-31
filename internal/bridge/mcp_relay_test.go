package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/securityboundary"
)

func TestMCPRelayPreservesVendorFramingAndCarriesExactAttestation(t *testing.T) {
	root := shortSocketTestRoot(t, "mr-")
	var calls atomic.Int32
	server, err := daemonpkg.StartControlServer(context.Background(), root, 11, func(_ context.Context, request daemonpkg.ControlRequest) (json.RawMessage, error) {
		calls.Add(1)
		var payload connectorRelayPayload
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Product != "claude" {
			t.Fatalf("product = %q", payload.Product)
		}
		switch request.Operation {
		case "connector.call":
			if request.AttachmentID != "attachment-native" || request.Capability != "capability-daemon" {
				t.Fatalf("control attestation = attachment %q capability %q", request.AttachmentID, request.Capability)
			}
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(payload.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.Arguments["session_id"] != "model-forgery" || payload.Evidence.ThreadID != "native-thread" {
				t.Fatalf("model/native evidence = %+v / %+v", params.Arguments, payload.Evidence)
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"delivered"}],"structuredContent":{"delivered":true}}`), nil
		default:
			t.Fatalf("unexpected operation %q", request.Operation)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	attestCalls := 0
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "claude", Endpoint: server.Endpoint(),
		Generation: func(context.Context) (uint64, error) { return 11, nil },
		Attest: func(_ context.Context, params json.RawMessage) (ConnectorAttestation, error) {
			attestCalls++
			if !strings.Contains(string(params), `"session_id":"model-forgery"`) {
				t.Fatalf("attested params = %s", params)
			}
			return ConnectorAttestation{
				AttachmentID: "attachment-native", Capability: "capability-daemon",
				Evidence: daemonpkg.NativeEvidence{Process: procinfo.Identity{PID: 812, Start: "start", StrongStart: "strong"}, RegistryPath: "/profile/native.json", SocketPath: "/profile/native.sock", ThreadID: "native-thread"},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"tools","method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_message","arguments":{"session_id":"model-forgery","message":"hello"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"ping","params":{}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := relay.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	responses := decodeRelayResponses(t, output.String())
	byID := make(map[string]relayResponse, len(responses))
	for _, response := range responses {
		byID[response.ID] = response
	}
	if len(responses) != 4 || byID["1"].ID == "" || byID[`"tools"`].ID == "" || byID["3"].ID == "" || byID["4"].ID == "" {
		t.Fatalf("response correlation = %+v", responses)
	}
	if attestCalls != 1 || calls.Load() != 1 {
		t.Fatalf("attestation calls=%d daemon calls=%d", attestCalls, calls.Load())
	}
	if !strings.Contains(string(byID["3"].Result), `"delivered":true`) {
		t.Fatalf("tool result = %s", byID["3"].Result)
	}
}

func TestMCPRelayBlockingToolDoesNotFreezeIndependentRequests(t *testing.T) {
	root := shortSocketTestRoot(t, "mr-")
	started, release := make(chan struct{}), make(chan struct{})
	server, err := daemonpkg.StartControlServer(context.Background(), root, 12, func(ctx context.Context, request daemonpkg.ControlRequest) (json.RawMessage, error) {
		if request.Operation != "connector.call" {
			return nil, fmt.Errorf("unexpected operation %q", request.Operation)
		}
		close(started)
		select {
		case <-release:
			return json.RawMessage(`{"content":[{"type":"text","text":"released"}]}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "codex", Endpoint: server.Endpoint(),
		Generation: func(context.Context) (uint64, error) { return 12, nil },
		Attest: func(context.Context, json.RawMessage) (ConnectorAttestation, error) {
			return ConnectorAttestation{AttachmentID: "thread", Evidence: daemonpkg.NativeEvidence{ThreadID: "thread"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, inputReader, outputWriter) }()
	lines := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(outputReader)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lane","arguments":{}}}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking tool never reached daemon")
	}
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`); err != nil {
		t.Fatal(err)
	}
	readID := func() string {
		t.Helper()
		select {
		case line := <-lines:
			var response struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				t.Fatal(err)
			}
			return string(response.ID)
		case <-time.After(time.Second):
			t.Fatal("relay response timed out")
			return ""
		}
	}
	if id := readID(); id != "2" {
		t.Fatalf("first response id = %s, want independent ping 2", id)
	}
	close(release)
	if id := readID(); id != "1" {
		t.Fatalf("released response id = %s, want blocking call 1", id)
	}
	_ = inputWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not finish after input closed")
	}
}

func TestMCPRelayBareCallsAreCanonicallyInactiveWithoutDaemonMutation(t *testing.T) {
	root := shortSocketTestRoot(t, "mr-")
	var calls atomic.Int32
	server, err := daemonpkg.StartControlServer(context.Background(), root, 4, func(_ context.Context, _ daemonpkg.ControlRequest) (json.RawMessage, error) {
		calls.Add(1)
		return nil, errors.New("unexpected daemon mutation")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "qwen", Endpoint: server.Endpoint(),
		Generation: func(context.Context) (uint64, error) { return 4, nil },
		Attest: func(context.Context, json.RawMessage) (ConnectorAttestation, error) {
			return ConnectorAttestation{}, ErrConnectorInactive
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}` + "\n"
	if err := relay.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	responses := decodeRelayResponses(t, output.String())
	if len(responses) != 1 || !strings.Contains(string(responses[0].Result), daemonpkg.CanonicalInactiveMessage) || !strings.Contains(string(responses[0].Result), `"isError":true`) {
		t.Fatalf("bare response = %+v", responses)
	}
	if calls.Load() != 0 {
		t.Fatalf("bare call reached daemon handler %d time(s)", calls.Load())
	}
}

func TestMCPRelayDiscoverySurvivesAbsentDaemon(t *testing.T) {
	root := shortSocketTestRoot(t, "mr-")
	endpoint, err := daemonpkg.ControlEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "codex", Endpoint: endpoint,
		Generation: func(context.Context) (uint64, error) { return 0, errors.New("daemon absent") },
		Attest: func(context.Context, json.RawMessage) (ConnectorAttestation, error) {
			return ConnectorAttestation{}, ErrConnectorInactive
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := relay.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	responses := decodeRelayResponses(t, output.String())
	byID := make(map[string]relayResponse, len(responses))
	for _, response := range responses {
		byID[response.ID] = response
	}
	if len(responses) != 2 || byID["1"].Error != nil || byID["2"].Error != nil ||
		!strings.Contains(string(byID["1"].Result), `"agent-sessions"`) ||
		!strings.Contains(string(byID["2"].Result), `"identity"`) {
		t.Fatalf("daemon-independent discovery = %+v", responses)
	}
}

func TestMCPRelayReconnectsAndRefreshesOnlyGenerationWithoutAdopting(t *testing.T) {
	root := shortSocketTestRoot(t, "mr-")
	var generation atomic.Uint64
	generation.Store(1)
	start := func(serverGeneration uint64) *daemonpkg.ControlServer {
		t.Helper()
		server, err := daemonpkg.StartControlServer(context.Background(), root, serverGeneration, func(_ context.Context, request daemonpkg.ControlRequest) (json.RawMessage, error) {
			body, err := json.Marshal(map[string]any{"generation": serverGeneration, "attachment": request.AttachmentID})
			return json.RawMessage(body), err
		})
		if err != nil {
			t.Fatal(err)
		}
		return server
	}
	server := start(1)
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "grok", Endpoint: server.Endpoint(),
		Generation: func(context.Context) (uint64, error) { return generation.Load(), nil },
		Attest: func(context.Context, json.RawMessage) (ConnectorAttestation, error) {
			return ConnectorAttestation{AttachmentID: "grok-native", Capability: "launch-capability", Evidence: daemonpkg.NativeEvidence{LeaderIdentity: "leader"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(id int) relayResponse {
		t.Helper()
		var output bytes.Buffer
		input := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`+"\n", id)
		if err := relay.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
			t.Fatal(err)
		}
		return decodeRelayResponses(t, output.String())[0]
	}
	if response := call(1); !strings.Contains(string(response.Result), `"generation":1`) {
		t.Fatalf("initial response = %+v", response)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	generation.Store(2)
	server = start(2)
	t.Cleanup(func() { _ = server.Close() })
	if response := call(2); !strings.Contains(string(response.Result), `"generation":2`) || !strings.Contains(string(response.Result), `"attachment":"grok-native"`) {
		t.Fatalf("reconnected response = %+v", response)
	}
}

func TestMCPRelayRejectsUnboundedOrUnknownFramesAndDoesNotLeakContent(t *testing.T) {
	canaries := securityboundary.FixtureCanaries()
	root := shortSocketTestRoot(t, "mr-")
	server, err := daemonpkg.StartControlServer(context.Background(), root, 6, func(context.Context, daemonpkg.ControlRequest) (json.RawMessage, error) {
		return nil, errors.New(string(canaries[securityboundary.LogContent]))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "codex", Endpoint: server.Endpoint(), MaxFrameBytes: 1024,
		Generation: func(context.Context) (uint64, error) { return 6, nil },
		Attest: func(context.Context, json.RawMessage) (ConnectorAttestation, error) {
			return ConnectorAttestation{AttachmentID: "codex-thread", Capability: "cap", Evidence: daemonpkg.NativeEvidence{ThreadID: "codex-thread"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`not-json`,
		`{"jsonrpc":"2.0","id":2,"method":"unknown","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_message","arguments":{"message":"` + string(canaries[securityboundary.PromptContent]) + `"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := relay.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	responses := decodeRelayResponses(t, output.String())
	byID := make(map[string]relayResponse, len(responses))
	for _, response := range responses {
		byID[response.ID] = response
	}
	if len(responses) != 3 || byID["null"].Error == nil || byID["null"].Error.Code != -32700 || byID["2"].Error == nil || byID["2"].Error.Code != -32601 {
		t.Fatalf("protocol errors = %+v", responses)
	}
	if found := securityboundary.Detect(output.Bytes(), canaries); len(found) != 0 {
		t.Fatalf("relay output leaked content canaries %v", found)
	}

	oversized := `{"jsonrpc":"2.0","id":9,"method":"ping","padding":"` + strings.Repeat("x", 2048) + `"}` + "\n"
	if err := relay.Serve(context.Background(), strings.NewReader(oversized), &bytes.Buffer{}); !errors.Is(err, ErrMCPRelayFrameTooLarge) {
		t.Fatalf("oversized frame error = %v", err)
	}
}

func TestMCPRelayShutdownDoesNotWaitForVendorStdin(t *testing.T) {
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "codex", Endpoint: filepath.Join(t.TempDir(), "daemon.sock"),
		Generation: func(context.Context) (uint64, error) { return 1, nil },
		Attest: func(context.Context, json.RawMessage) (ConnectorAttestation, error) {
			return ConnectorAttestation{}, ErrConnectorInactive
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, reader, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay shutdown waited for vendor stdin")
	}
}

func TestMCPRelayRefreshesOnlyWhileWaitingForVendorFrame(t *testing.T) {
	var refreshes atomic.Int32
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "codex", Endpoint: filepath.Join(t.TempDir(), "daemon.sock"),
		Generation: func(context.Context) (uint64, error) { return 1, nil },
		Attest: func(context.Context, json.RawMessage) (ConnectorAttestation, error) {
			return ConnectorAttestation{}, ErrConnectorInactive
		},
		RefreshEvery: time.Millisecond,
		Refresh: func(context.Context) error {
			refreshes.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, reader, io.Discard) }()
	deadline := time.Now().Add(time.Second)
	for refreshes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if refreshes.Load() == 0 {
		t.Fatal("idle relay did not check its installed image")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not stop after refresh check")
	}
}

type relayResponse struct {
	ID     string
	Result json.RawMessage
	Error  *rpcError
}

func decodeRelayResponses(t *testing.T, output string) []relayResponse {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	responses := make([]relayResponse, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("decode relay response %q: %v", line, err)
		}
		responses = append(responses, relayResponse{ID: string(raw.ID), Result: raw.Result, Error: raw.Error})
	}
	return responses
}
