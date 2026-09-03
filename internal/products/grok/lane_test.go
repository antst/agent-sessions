package grok

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

const testNativeID = "01a06515-4dd7-7fe3-b0fa-63749ce9e1c7"

func grokTestMessage(body string) productruntime.NativeMessage {
	return productruntime.NativeMessage{ID: "message", Body: body, From: productruntime.NativeMessageSource{
		UUID: "parent", Name: "parent", Product: "claude", Groups: []string{"team"},
	}}
}

func renderGrokTestMessage(t *testing.T, body string) string {
	t.Helper()
	rendered, err := sessiontools.RenderNativeMessage(grokTestMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}

type fakeFactory struct {
	request NativeOpenRequest
	session *fakeSession
	calls   int
}

func (factory *fakeFactory) Open(_ context.Context, request NativeOpenRequest) (NativeSession, error) {
	factory.calls++
	factory.request = request
	return factory.session, nil
}

type fakeSession struct {
	mu            sync.Mutex
	id            string
	models        []string
	modes         []string
	prompts       []string
	interjections [][2]string
	cancels       int
	closes        int
	result        NativePromptResult
	promptErr     error
}

func (session *fakeSession) NativeID() string { return session.id }
func (session *fakeSession) SetModel(_ context.Context, model string) error {
	session.models = append(session.models, model)
	return nil
}
func (session *fakeSession) SetMode(_ context.Context, mode string) error {
	session.modes = append(session.modes, mode)
	return nil
}
func (session *fakeSession) StartPrompt(_ context.Context, prompt string) (NativePrompt, error) {
	session.prompts = append(session.prompts, prompt)
	return fakePrompt{result: session.result, err: session.promptErr}, nil
}
func (session *fakeSession) Interject(_ context.Context, id, message string) error {
	session.mu.Lock()
	session.interjections = append(session.interjections, [2]string{id, message})
	session.mu.Unlock()
	return nil
}
func (session *fakeSession) Cancel() error { session.cancels++; return nil }
func (session *fakeSession) Close()        { session.closes++ }

type fakePrompt struct {
	result NativePromptResult
	err    error
}

func (prompt fakePrompt) Wait(context.Context) (NativePromptResult, error) {
	return prompt.result, prompt.err
}

func testDriver(t *testing.T, session *fakeSession) (*LaneDriver, *fakeFactory) {
	t.Helper()
	descriptor, ok := productcatalog.ByID(ProductID)
	if !ok {
		t.Fatal("Grok descriptor unavailable")
	}
	factory := &fakeFactory{session: session}
	driver, err := NewLaneDriver(LaneConfig{
		Descriptor: descriptor, Generation: 7, Native: factory,
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver, factory
}

func openRequest() productruntime.LaneOpenRequest {
	return productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "lane-1", Name: "named-grok", Cwd: "/work",
		Capability: "lane-capability", Groups: []string{"parent", "parent/child"},
		Environment:    []string{"PATH=/bin", "AGENT_SESSIONS_SESSION_ID=lane-1"},
		PermissionMode: permissionmode.BypassPermissions,
	}
}

func TestLaneOpenCarriesNativeFactsAndAppliesOnlyRequestedModel(t *testing.T) {
	session := &fakeSession{id: testNativeID}
	driver, factory := testDriver(t, session)
	request := openRequest()
	request.Arguments = []string{"--model", "grok-4.5", "--agent", "plan", "--tools", "shell"}
	ref, err := driver.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if ref.LaneID != testNativeID || ref.NativeSessionID != testNativeID || ref.Generation != 7 {
		t.Fatalf("ref = %#v", ref)
	}
	split := ref
	split.LaneID = request.LaneID
	if err := driver.SendMessage(context.Background(), split, grokTestMessage("must not deliver")); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("provisional identity message error = %v", err)
	}
	if factory.calls != 1 || factory.request.Name != "named-grok" || factory.request.Capability != "lane-capability" ||
		!reflect.DeepEqual(factory.request.Groups, request.Groups) || !reflect.DeepEqual(factory.request.Environment, request.Environment) ||
		!reflect.DeepEqual(factory.request.Arguments, []string{"--agent", "plan", "--tools", "shell"}) {
		t.Fatalf("native open = %#v", factory.request)
	}
	if !reflect.DeepEqual(session.models, []string{"grok-4.5"}) || len(session.modes) != 0 {
		t.Fatalf("models=%v modes=%v", session.models, session.modes)
	}
}

func TestLaneOpenRejectsAnythingButExplicitYolo(t *testing.T) {
	session := &fakeSession{id: testNativeID}
	driver, factory := testDriver(t, session)
	request := openRequest()
	request.PermissionMode = permissionmode.Default
	if _, err := driver.Open(context.Background(), request); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("error = %v", err)
	}
	if factory.calls != 0 {
		t.Fatalf("native opens = %d", factory.calls)
	}
}

func TestLaneTurnUsesNativeModeAndProductTerminal(t *testing.T) {
	session := &fakeSession{id: testNativeID, result: NativePromptResult{Output: "native answer", StopReason: "end_turn"}}
	driver, _ := testDriver(t, session)
	ref, err := driver.Open(context.Background(), openRequest())
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), ref, productruntime.TurnStartRequest{
		Prompt: "work", PermissionMode: permissionmode.BypassPermissions, Effort: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "native answer" || terminal.NativeStopReason != "end_turn" {
		t.Fatalf("terminal = %#v", terminal)
	}
	if !reflect.DeepEqual(session.modes, []string{"medium"}) || !reflect.DeepEqual(session.prompts, []string{"work"}) {
		t.Fatalf("modes=%v prompts=%v", session.modes, session.prompts)
	}
}

func TestLaneInterruptIsProductTerminalAndSessionRemainsReusable(t *testing.T) {
	session := &fakeSession{id: testNativeID, result: NativePromptResult{Output: "partial", StopReason: "cancelled"}}
	driver, _ := testDriver(t, session)
	ref, err := driver.Open(context.Background(), openRequest())
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), ref, productruntime.TurnStartRequest{Prompt: "long", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Interrupt(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Outcome != productruntime.TurnInterrupted || terminal.ExitLike != 130 || session.cancels != 1 {
		t.Fatalf("terminal=%#v cancels=%d", terminal, session.cancels)
	}
	session.result = NativePromptResult{Output: "after cancel", StopReason: "end_turn"}
	next, err := driver.StartTurn(context.Background(), ref, productruntime.TurnStartRequest{Prompt: "again", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.WaitTurn(context.Background(), next); err != nil {
		t.Fatal(err)
	}
}

func TestSteerAndMessageShareNativeInterjection(t *testing.T) {
	session := &fakeSession{id: testNativeID, result: NativePromptResult{StopReason: "end_turn"}}
	driver, _ := testDriver(t, session)
	ref, err := driver.Open(context.Background(), openRequest())
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), ref, productruntime.TurnStartRequest{Prompt: "long", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{Prompt: "change", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.SendMessage(context.Background(), ref, grokTestMessage("inbound")); err != nil {
		t.Fatal(err)
	}
	if accepted.NativeSessionID != testNativeID || accepted.NativeMessageID == "" || len(session.interjections) != 2 ||
		session.interjections[0][1] != "change" || session.interjections[1][1] != renderGrokTestMessage(t, "inbound") {
		t.Fatalf("accepted=%#v interjections=%v", accepted, session.interjections)
	}
}

func TestExactResumeReusesOneNativeOwnerAndArchiveClosesIt(t *testing.T) {
	session := &fakeSession{id: testNativeID}
	driver, factory := testDriver(t, session)
	request := openRequest()
	request.ResumeNativeID = testNativeID
	request.LaneID = testNativeID
	ref, err := driver.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Open(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if factory.calls != 1 {
		t.Fatalf("native opens = %d", factory.calls)
	}
	if err := driver.Archive(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if session.closes != 1 {
		t.Fatalf("native closes = %d", session.closes)
	}
}
