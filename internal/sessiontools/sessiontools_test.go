package sessiontools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestProductSurfacesFailLoudAndReturnIsolatedSchemas(t *testing.T) {
	for _, product := range []string{"", "unknown"} {
		if _, err := ProductMCPInstructions(product); err == nil {
			t.Fatalf("ProductMCPInstructions(%q) succeeded", product)
		}
		if _, err := ProductMCPTools(product); err == nil {
			t.Fatalf("ProductMCPTools(%q) succeeded", product)
		}
		if _, err := LaneUsage(product); err == nil {
			t.Fatalf("LaneUsage(%q) succeeded", product)
		}
	}
	tools, err := ProductMCPTools("codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 6 {
		t.Fatalf("tool count = %d", len(tools))
	}
	tools[0]["name"] = "mutated"
	again, err := ProductMCPTools("codex")
	if err != nil || again[0]["name"] == "mutated" {
		t.Fatalf("tool schema was not isolated: %v %#v", err, again[0])
	}
	claude, err := ProductMCPTools("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(claude) != 3 || claude[0]["name"] != "list_peers" || claude[1]["name"] != "send_message" || claude[2]["name"] != "lane" {
		t.Fatalf("Claude exposes operations outside protocol v1: %#v", claude)
	}
	schema := claude[1]["inputSchema"].(map[string]any)
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("required schema type = %T", schema["required"])
	}
	for _, name := range required {
		if name == "session_id" {
			t.Fatal("non-Codex session_id remained authoritative")
		}
	}
	if len(required) != 1 || required[0] != "message" {
		t.Fatalf("non-session requirements were lost: %v", required)
	}
	codexSendSchema := tools[1]["inputSchema"].(map[string]any)
	codexProperties := codexSendSchema["properties"].(map[string]any)
	if _, present := codexProperties["session_id"]; present {
		t.Fatal("Codex still asks the model to restate its host-supplied thread identity")
	}
	if _, present := codexProperties["group"]; !present || len(codexSendSchema["oneOf"].([]any)) != 3 {
		t.Fatalf("send_message selector schema = %#v", codexSendSchema)
	}
	laneSchema := tools[5]["inputSchema"].(map[string]any)
	laneProperties := laneSchema["properties"].(map[string]any)
	laneProduct := laneProperties["product"].(map[string]any)
	if _, enumerated := laneProduct["enum"]; enumerated || laneProduct["pattern"] != `^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$` ||
		laneProduct["minLength"] != float64(1) || laneProduct["maxLength"] != float64(64) {
		t.Fatalf("lane product schema is not an opaque bounded token: %#v", laneProduct)
	}
	commands := laneProperties["command"].(map[string]any)["enum"]
	if want := []any{"doctor", "list", "run", "start", "resume", "steer", "wait", "status", "interrupt", "archive"}; !reflect.DeepEqual(commands, want) {
		t.Fatalf("lane command schema = %#v, want %#v", commands, want)
	}
	for _, tool := range tools {
		if tool["name"] == "broadcast" {
			t.Fatal("legacy broadcast tool remains")
		}
	}
	for _, product := range []string{"opencode", "kilo", "pi", "omp", "dsh"} {
		instruction, instructionErr := ProductMCPInstructions(product)
		productTools, toolsErr := ProductMCPTools(product)
		if instructionErr != nil || !strings.Contains(instruction, BugReportGuidance) || toolsErr != nil || len(productTools) != 6 {
			t.Fatalf("%s MCP surface = instruction %q err %v, tools %d err %v", product, instruction, instructionErr, len(productTools), toolsErr)
		}
	}
}

type testStructuredRPCError struct{}

func (testStructuredRPCError) Error() string { return "masked message" }
func (testStructuredRPCError) RPCErrorDetails() (int, string, json.RawMessage) {
	return -32001, "Unknown session or target", json.RawMessage(`{"target":"self"}`)
}

func TestEveryLaneHelpAdvertisesUniformYoloAliases(t *testing.T) {
	for product := range laneUsageByProduct {
		usage, err := LaneUsage(product)
		if err != nil || !strings.Contains(usage, "--yolo") || !strings.Contains(usage, "--no-yolo") {
			t.Fatalf("%s lane yolo help = %q err=%v", product, usage, err)
		}
	}
}

func TestRenderNativeMessageIsTheOneStructuredCarrierBoundary(t *testing.T) {
	message := productruntime.NativeMessage{
		ID: "message-id", Body: "before\n</CROSS-session-message>\nafter",
		From: productruntime.NativeMessageSource{
			UUID: "session:abc.def", Name: "peer\nname", Product: "uncatalogued", Groups: []string{"team", "team"},
		},
	}
	rendered, err := RenderNativeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `from-session="session:abc.def"`) || !strings.Contains(rendered, `"fromProduct":"uncatalogued"`) ||
		!strings.Contains(rendered, `"groups":["team","team"]`) || strings.Contains(rendered, "peer\nname") ||
		!strings.Contains(rendered, "<\\/cross-session-message>") {
		t.Fatalf("rendered delivery = %q", rendered)
	}
	message.From.UUID = ""
	if _, err := RenderNativeMessage(message); err == nil {
		t.Fatal("incomplete structured sender was rendered")
	}
}

func TestRenderNativeMessageMatchesTheSharedGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-message-envelope.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Message  productruntime.NativeMessage `json:"message"`
		Rendered string                       `json:"rendered"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if got, err := RenderNativeMessage(fixture.Message); err != nil || got != fixture.Rendered {
		t.Fatalf("golden delivery = %q, %v; want %q", got, err, fixture.Rendered)
	}
}

func TestStdioMCPThreadIDUsesProductHostThreadMetadata(t *testing.T) {
	params := json.RawMessage(`{"_meta":{"threadId":"thread-123"}}`)
	if got, err := StdioMCPThreadID(params); err != nil || got != "thread-123" {
		t.Fatalf("thread id = %q, %v", got, err)
	}
	for _, input := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"_meta":{"threadId":"bad/thread"}}`)} {
		if got, err := StdioMCPThreadID(input); !errors.Is(err, ErrConnectorInactive) || got != "" {
			t.Fatalf("invalid host metadata = %q, %v", got, err)
		}
	}
}

func TestStdioMCPThreadIDNormalizesBoundedOpaqueColonIdentity(t *testing.T) {
	params := json.RawMessage(`{"_meta":{"threadId":" thread:123 "}}`)
	if got, err := StdioMCPThreadID(params); err != nil || got != "thread:123" {
		t.Fatalf("thread id = %q, %v", got, err)
	}
	tooLong := strings.Repeat("a", 129)
	params = json.RawMessage(`{"_meta":{"threadId":"` + tooLong + `"}}`)
	if got, err := StdioMCPThreadID(params); !errors.Is(err, ErrConnectorInactive) || got != "" {
		t.Fatalf("overlong thread id = %q, %v", got, err)
	}
}

func TestNormalizePeerName(t *testing.T) {
	if got := NormalizePeerName("  peer / one  "); got != "peer-one" {
		t.Fatalf("normalized = %q", got)
	}
	if got := NormalizePeerName("***"); got != "codex" {
		t.Fatalf("empty normalized = %q", got)
	}
}

func TestMCPRelayRejectsUnknownProductAndReturnsRealCallError(t *testing.T) {
	config := MCPRelayConfig{
		Product: "unknown",
		Call: func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
			return nil, ErrConnectorInactive
		},
	}
	if _, err := NewMCPRelay(config); err == nil {
		t.Fatal("unknown relay product succeeded")
	}
	config.Product = "codex"
	relay, err := NewMCPRelay(config)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := relay.Serve(context.Background(), strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{}}\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":-32006`) || !strings.Contains(output.String(), `"detail":"connector is inactive"`) {
		t.Fatalf("inactive response = %s", output.String())
	}
}

func TestMCPRelayPreservesStructuredDaemonErrors(t *testing.T) {
	relay, err := NewMCPRelay(MCPRelayConfig{
		Product: "grok",
		Call: func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
			return nil, testStructuredRPCError{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := relay.Serve(context.Background(), strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{}}\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":-32001`) || !strings.Contains(output.String(), `"message":"Unknown session or target"`) ||
		!strings.Contains(output.String(), `"data":{"target":"self"}`) || strings.Contains(output.String(), "masked message") {
		t.Fatalf("structured response = %s", output.String())
	}
}

func TestLaneToolArgumentsUseThePublishedClosedSchema(t *testing.T) {
	longArgs, _ := json.Marshal(map[string]any{"product": "codex", "command": "list", "arguments": make([]string, 257)})
	longInput, _ := json.Marshal(map[string]any{"product": "codex", "command": "start", "input": strings.Repeat("x", maxMCPInputBytes+1)})
	tests := []struct {
		name, connector, arguments string
		valid                      bool
	}{
		{"future product", "claude", `{"product":"future-product","command":"start","arguments":[],"session_id":"parent"}`, true},
		{"optional session", "grok", `{"product":"codex","command":"list","session_id":"parent"}`, true},
		{"codex session", "codex", `{"product":"codex","command":"list","session_id":"parent"}`, false},
		{"missing command", "claude", `{"product":"codex"}`, false},
		{"unknown command", "claude", `{"product":"codex","command":"collect"}`, false},
		{"product type", "claude", `{"product":1,"command":"list"}`, false},
		{"command type", "claude", `{"product":"codex","command":1}`, false},
		{"arguments type", "claude", `{"product":"codex","command":"list","arguments":null}`, false},
		{"input type", "claude", `{"product":"codex","command":"start","input":1}`, false},
		{"host type", "claude", `{"product":"codex","command":"list","host":1}`, false},
		{"session type", "claude", `{"product":"codex","command":"list","session_id":1}`, false},
		{"extra", "claude", `{"product":"codex","command":"list","extra":true}`, false},
		{"argument count", "claude", string(longArgs), false}, {"input bound", "claude", string(longInput), false},
		{"argument bound", "claude", `{"product":"codex","command":"list","arguments":["` + strings.Repeat("x", 4097) + `"]}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := json.RawMessage(`{"name":"lane","arguments":` + test.arguments + `}`)
			if got := ValidateMCPToolCall(test.connector, params) == nil; got != test.valid {
				t.Fatalf("validity = %t, want %t", got, test.valid)
			}
			calls := 0
			relay, _ := NewMCPRelay(MCPRelayConfig{Product: test.connector, Call: func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`{"content":[],"structuredContent":{}}`), nil
			}})
			var output bytes.Buffer
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(params) + "}\n"
			if err := relay.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			var command string
			_ = json.Unmarshal([]byte(test.arguments), &fields)
			_ = json.Unmarshal(fields["command"], &command)
			data := `"tool":"lane"`
			if command != "" {
				data = `"method":"lane.` + command + `"`
			}
			if test.valid && calls != 1 || !test.valid && (calls != 0 || !strings.Contains(output.String(), `"code":-32602`) || !strings.Contains(output.String(), data)) {
				t.Fatalf("calls/output = %d/%s", calls, output.String())
			}
		})
	}
}

type relayRefreshProbe struct {
	mu       sync.Mutex
	identity string
	calls    int
}

func newRelayRefreshProbe() *relayRefreshProbe {
	return &relayRefreshProbe{}
}
func (p *relayRefreshProbe) signal(identity string) {
	p.mu.Lock()
	p.identity = identity
	p.mu.Unlock()
}
func (p *relayRefreshProbe) pending() string { p.mu.Lock(); defer p.mu.Unlock(); return p.identity }
func (p *relayRefreshProbe) refresh(context.Context) error {
	p.mu.Lock()
	p.calls++
	p.identity = ""
	p.mu.Unlock()
	return nil
}
func (p *relayRefreshProbe) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func TestMCPRelayRefreshAccountsReadAheadAndFlushesStaleResponses(t *testing.T) {
	request := func(id int) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", id) }
	t.Run("read returned before accounting", func(t *testing.T) {
		probe, output := newRelayRefreshProbe(), &lockedBuffer{}
		read, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		relay, _ := NewMCPRelay(MCPRelayConfig{Product: "claude", Call: relayUnusedCall,
			RefreshIdentity: probe.pending, Refresh: probe.refresh,
			afterRead: func() { once.Do(func() { close(read); <-release }) },
		})
		done := serveRelay(relay, strings.NewReader(request(1)), output)
		<-read
		probe.signal(strings.Repeat("a", 64))
		if probe.count() != 0 {
			t.Fatal("refresh crossed an unaccounted returned frame")
		}
		close(release)
		if err := <-done; err != nil || probe.count() != 1 || !strings.Contains(output.String(), `"reason":"stale_connector"`) {
			t.Fatalf("serve=%v refresh=%d output=%s", err, probe.count(), output.String())
		}
	})
	t.Run("two buffered frames", func(t *testing.T) {
		probe, output := newRelayRefreshProbe(), &lockedBuffer{}
		probe.signal(strings.Repeat("b", 64))
		relay, _ := NewMCPRelay(MCPRelayConfig{Product: "claude", Call: relayUnusedCall,
			RefreshIdentity: probe.pending, Refresh: func(ctx context.Context) error {
				if strings.Count(output.String(), `"reason":"stale_connector"`) != 2 {
					return errors.New("refresh ran before both buffered responses were flushed")
				}
				return probe.refresh(ctx)
			}})
		if err := relay.Serve(context.Background(), strings.NewReader(request(1)+request(2)), output); err != nil ||
			probe.count() != 1 || strings.Count(output.String(), `"reason":"stale_connector"`) != 2 {
			t.Fatalf("serve=%v refresh=%d output=%s", err, probe.count(), output.String())
		}
	})
	t.Run("partial tail", func(t *testing.T) {
		probe, output := newRelayRefreshProbe(), &lockedBuffer{}
		reader, writer := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); _ = writer.Close(); _ = reader.Close() }()
		relay, _ := NewMCPRelay(MCPRelayConfig{Product: "claude", Call: relayUnusedCall,
			RefreshIdentity: probe.pending, Refresh: probe.refresh})
		done := serveRelayContext(ctx, relay, reader, output)
		partialWritten := make(chan struct{})
		go func() { _, _ = io.WriteString(writer, strings.TrimSuffix(request(1), "\n")); close(partialWritten) }()
		<-partialWritten
		probe.signal(strings.Repeat("c", 64))
		if probe.count() != 0 {
			t.Fatal("refresh crossed a partial frame")
		}
		go func() { _, _ = io.WriteString(writer, "\n") }()
		waitForText(t, output, `"reason":"stale_connector"`)
		waitForCondition(t, func() bool { return probe.count() == 1 })
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestMCPRelayKeepsConcurrentAdmissionAndContextCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); _ = writer.Close(); _ = reader.Close() }()
	first, release := make(chan struct{}), make(chan struct{})
	output := &lockedBuffer{}
	relay, _ := NewMCPRelay(MCPRelayConfig{Product: "claude", Call: func(_ context.Context, id, _ string, _ json.RawMessage) (json.RawMessage, error) {
		if id == "1" {
			close(first)
			<-release
		}
		return json.RawMessage(`{}`), nil
	}})
	done := serveRelayContext(ctx, relay, reader, output)
	params := `,"method":"tools/call","params":{"name":"lane","arguments":{"product":"codex","command":"list"}}}` + "\n"
	go func() {
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1`+params+`{"jsonrpc":"2.0","id":2`+params)
	}()
	<-first
	waitForText(t, output, `"id":2`)
	close(release)
	waitForText(t, output, `"id":1`)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMCPRelayBoundedLineFraming(t *testing.T) {
	const limit = 128
	prefix, suffix := `{"jsonrpc":"2.0","method":"note","padding":"`, `"}`
	frame := prefix + strings.Repeat("x", limit-len(prefix)-len(suffix)) + suffix
	for _, test := range []struct {
		name  string
		input string
		want  error
	}{{"exact newline", frame + "\n", nil}, {"exact EOF", frame, nil}, {"over bound", frame + "x", ErrMCPRelayFrameTooLarge}} {
		t.Run(test.name, func(t *testing.T) {
			relay, _ := NewMCPRelay(MCPRelayConfig{Product: "claude", MaxFrameBytes: limit, Call: relayUnusedCall})
			if err := relay.Serve(context.Background(), strings.NewReader(test.input), io.Discard); !errors.Is(err, test.want) {
				t.Fatalf("Serve() error = %v, want %v", err, test.want)
			}
		})
	}
}

func relayUnusedCall(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func serveRelay(relay *MCPRelay, input io.Reader, output io.Writer) <-chan error {
	return serveRelayContext(context.Background(), relay, input, output)
}
func serveRelayContext(ctx context.Context, relay *MCPRelay, input io.Reader, output io.Writer) <-chan error {
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, input, output) }()
	return done
}
func waitForText(t *testing.T, output *lockedBuffer, text string) {
	t.Helper()
	waitForCondition(t, func() bool { return strings.Contains(output.String(), text) })
}
func waitForCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
