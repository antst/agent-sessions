package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestLaneCommandMapsCanonicalLifecycleToDaemonLaneEngine(t *testing.T) {
	fixture, engine, adapter, parent := newLaneCommandTestFixture(t)
	secretPrompt := "T063_PROMPT_MUST_NOT_APPEAR_IN_PUBLIC_RESULT"
	started := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "claude", Command: "start", Input: secretPrompt,
		Arguments: []string{"--name", "reviewer", "-C", "child", "-g", "child-group", "--inherit-groups", "--persistent", "--notify", "owner", "--model", "opus", "--effort", "high"},
	})
	lane, ok := started["lane"].(LaneRecord)
	if !ok || lane.ParentSessionID != parent.SessionID || lane.Cwd != "/workspace/child" ||
		lane.PermissionMode != "dontAsk" || !lane.InheritParentGroups ||
		!containsLaneString(lane.Groups, "project") || !containsLaneString(lane.Groups, "child-group") {
		t.Fatalf("started lane = %#v", started["lane"])
	}
	if body, err := json.Marshal(started); err != nil || strings.Contains(string(body), secretPrompt) {
		t.Fatalf("public start result leaked prompt: %s, %v", body, err)
	}
	turn, err := engine.latestLaneTurn(context.Background(), lane.LaneSessionID, parent.AttachmentID)
	if err != nil || turn.InputReference["prompt"] != secretPrompt {
		t.Fatalf("durable daemon input = %#v, %v", turn.InputReference, err)
	}

	status := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "claude", Command: "status", Arguments: []string{"reviewer"},
	})
	if status["lane"].(LaneRecord).LaneSessionID != lane.LaneSessionID {
		t.Fatalf("status = %#v", status)
	}
	listed := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "claude", Command: "list", Arguments: []string{"--mine"},
	})
	if got := listed["lanes"].([]LaneRecord); len(got) != 1 || got[0].LaneSessionID != lane.LaneSessionID {
		t.Fatalf("list = %#v", listed)
	}
	interrupted := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "claude", Command: "interrupt", Arguments: []string{lane.LaneSessionID},
	})
	if interrupted["turn"].(LaneTurnRecord).TerminalOutcome != LaneTerminalInterrupted || adapter.interruptCount() != 1 {
		t.Fatalf("interrupt = %#v, adapter calls %d", interrupted, adapter.interruptCount())
	}
	waited := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "claude", Command: "wait", Arguments: []string{"reviewer", "--timeout", "1"},
	})
	if waited["collection"].(LaneCollection).Outcome != LaneTerminalInterrupted {
		t.Fatalf("wait = %#v", waited)
	}

	adapter.setAutoComplete(true)
	resumed := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "claude", Command: "resume", Arguments: []string{"reviewer", "--model", "sonnet", "--timeout", "1"}, Input: "follow up",
	})
	if resumed["collection"].(LaneCollection).Outcome != LaneTerminalCompleted {
		t.Fatalf("resume = %#v", resumed)
	}
	archived := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "claude", Command: "archive", Arguments: []string{"reviewer"},
	})
	if archived["lane"].(LaneRecord).State != LaneStateArchived || adapter.archiveCount() != 1 {
		t.Fatalf("archive = %#v, adapter calls %d", archived, adapter.archiveCount())
	}

	run := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "codex", Command: "run", Arguments: []string{"--name", "implementer", "--timeout", "1"}, Input: "implement",
	})
	if run["collection"].(LaneCollection).Outcome != LaneTerminalCompleted {
		t.Fatalf("run = %#v", run)
	}
}

func TestLaneCommandRejectsDuplicateAndAmbiguousNames(t *testing.T) {
	fixture, engine, _, parent := newLaneCommandTestFixture(t)
	request := LaneCommandRequest{Product: "grok", Command: "start", Arguments: []string{"--name", "duplicate"}, Input: "one"}
	executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, request)
	request.Input = "two"
	if _, err := executeLaneCommand(context.Background(), engine, fixture.attachments, parent.AttachmentID, request); !errors.Is(err, ErrLaneIdempotencyConflict) {
		t.Fatalf("duplicate name error = %v", err)
	}
	request.Arguments = append(request.Arguments, "--allow-duplicate-name")
	executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, request)
	if _, err := executeLaneCommand(context.Background(), engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "grok", Command: "status", Arguments: []string{"duplicate"},
	}); !errors.Is(err, ErrLaneIdempotencyConflict) {
		t.Fatalf("ambiguous name error = %v", err)
	}
}

func TestLaneCommandPreservesValidatedProductOptionInventories(t *testing.T) {
	fixture, engine, _, parent := newLaneCommandTestFixture(t)
	for _, test := range []struct {
		product, permission string
		arguments           []string
	}{
		{product: "codex", permission: "bypassPermissions", arguments: []string{"--model", "gpt-test", "--effort", "high", "--sandbox", "danger-full-access", "--approval-policy", "never", "--config", "a=b", "--web", "--schema", "result.json", "--worktree", "--skip-git-repo-check"}},
		{product: "claude", permission: "plan", arguments: []string{"--model", "opus", "--effort", "high", "--permission-mode", "plan", "--max-budget-usd", "2.5", "--tools", "Bash", "--allowed-tools", "Read", "--disallowed-tools", "Write", "--schema", "result.json", "--worktree"}},
		{product: "grok", permission: "bypassPermissions", arguments: []string{"--model", "grok-test", "--reasoning-effort", "high", "--permission-mode", "bypassPermissions"}},
		{product: "qwen", permission: "yolo", arguments: []string{"--qwen-home", "/tmp/qwen-test", "--yolo"}},
	} {
		t.Run(test.product, func(t *testing.T) {
			arguments := append([]string{"--name", test.product + "-worker"}, test.arguments...)
			result := executeLaneCommandTest(t, engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
				Product: test.product, Command: "start", Arguments: arguments, Input: "work",
			})
			lane := result["lane"].(LaneRecord)
			if lane.PermissionMode != test.permission {
				t.Fatalf("permission = %q, want %q", lane.PermissionMode, test.permission)
			}
			turn, err := engine.latestLaneTurn(context.Background(), lane.LaneSessionID, parent.AttachmentID)
			if err != nil {
				t.Fatal(err)
			}
			options, _ := turn.InputReference["options"].(map[string]any)
			if got := laneCommandTestStrings(options["arguments"]); strings.Join(got, "\x00") != strings.Join(arguments, "\x00") {
				t.Fatalf("durable canonical arguments = %#v, want %#v", options["arguments"], arguments)
			}
		})
	}
}

func laneCommandTestStrings(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func TestLaneCommandBoundsInputAndControlInjectsAttestedPrincipal(t *testing.T) {
	fixture, engine, adapter, parent := newLaneCommandTestFixture(t)
	if _, err := executeLaneCommand(context.Background(), engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "qwen", Command: "start", Arguments: []string{"--name", "large"}, Input: strings.Repeat("x", maxLaneCommandInputBytes+1),
	}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized input error = %v", err)
	}
	runtime := &Runtime{attachments: fixture.attachments, lanes: engine}
	dispatch := runtimeControlDispatch(runtime)
	attack := json.RawMessage(`{"product":"qwen","command":"start","arguments":["--name","forged"],"input":"work","source_attachment_id":"other-parent"}`)
	if _, failure := dispatch(context.Background(), controlPrincipal{
		Role: controlRoleLauncher, AttachmentID: parent.AttachmentID, SessionID: parent.SessionID, Attested: true,
	}, controlRequest{Operation: "lane.command", Payload: attack}); failure == nil || failure.Code != "invalid_payload" {
		t.Fatalf("claimed parent injection failure = %#v", failure)
	}
	if adapter.dispatchCount() != 0 {
		t.Fatalf("rejected claimed parent dispatched %d turns", adapter.dispatchCount())
	}
	payload, err := json.Marshal(LaneCommandRequest{Product: "qwen", Command: "start", Arguments: []string{"--name", "attested"}, Input: "work"})
	if err != nil {
		t.Fatal(err)
	}
	result, failure := dispatch(context.Background(), controlPrincipal{
		Role: controlRoleLauncher, AttachmentID: parent.AttachmentID, SessionID: parent.SessionID, Attested: true,
	}, controlRequest{Operation: "lane.command", Payload: payload})
	if failure != nil || len(result.Result) == 0 || adapter.dispatchCount() != 1 {
		t.Fatalf("attested dispatch = %#v, failure %#v, calls %d", result, failure, adapter.dispatchCount())
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.Result, &decoded); err != nil {
		t.Fatal(err)
	}
	lane, _ := decoded["lane"].(map[string]any)
	if lane["parent_session_id"] != parent.SessionID {
		t.Fatalf("daemon-selected parent = %#v", lane)
	}
}

type laneCommandTestAdapter struct {
	mu           sync.Mutex
	engine       *LaneEngine
	autoComplete bool
	dispatches   int
	interrupts   int
	archives     int
}

func (adapter *laneCommandTestAdapter) Dispatch(_ context.Context, lane LaneRecord, turn LaneTurnRecord) (LaneDispatchResult, error) {
	adapter.mu.Lock()
	adapter.dispatches++
	auto, engine := adapter.autoComplete, adapter.engine
	adapter.mu.Unlock()
	if auto {
		go func() {
			_, _ = engine.Complete(context.Background(), LaneTerminalRequest{
				LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, Outcome: LaneTerminalCompleted,
				NativeTurnIdentity: map[string]any{"turn": turn.TurnID}, ResultReference: map[string]any{"reference": "terminal"},
			})
		}()
	}
	return LaneDispatchResult{NativeActor: map[string]any{"lane": lane.LaneSessionID}, NativeTurnIdentity: map[string]any{"turn": turn.TurnID}}, nil
}

func (adapter *laneCommandTestAdapter) InterruptTurn(context.Context, LaneRecord, LaneTurnRecord) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.interrupts++
	return nil
}

func (adapter *laneCommandTestAdapter) Archive(context.Context, LaneRecord) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.archives++
	return nil
}

func (adapter *laneCommandTestAdapter) setAutoComplete(value bool) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.autoComplete = value
}

func (adapter *laneCommandTestAdapter) dispatchCount() int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.dispatches
}

func (adapter *laneCommandTestAdapter) interruptCount() int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.interrupts
}

func (adapter *laneCommandTestAdapter) archiveCount() int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.archives
}

func newLaneCommandTestFixture(t *testing.T) (*laneTestFixture, *LaneEngine, *laneCommandTestAdapter, AttachmentRecord) {
	t.Helper()
	fixture := newLaneTestFixture(t, nil, nil)
	adapter := &laneCommandTestAdapter{}
	adapters := make(map[string]LaneAdapter)
	for _, product := range productcatalog.ProductDescriptors() {
		adapters[product.ID] = adapter
	}
	options := fixture.options()
	options.Adapters = adapters
	engine, err := NewLaneEngine(options)
	if err != nil {
		t.Fatal(err)
	}
	adapter.engine = engine
	parent := fixture.attach(t, "codex", "parent-session", []string{"project"})
	return fixture, engine, adapter, parent
}

func executeLaneCommandTest(t *testing.T, engine *LaneEngine, attachments *AttachmentRegistry, attachmentID string, request LaneCommandRequest) map[string]any {
	t.Helper()
	result, err := executeLaneCommand(context.Background(), engine, attachments, attachmentID, request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
