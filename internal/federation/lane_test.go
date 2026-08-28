package federation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestRemoteLaneDispatchCallsDestinationAuthorityDirectlyForEveryProduct(t *testing.T) {
	for _, product := range productcatalog.ProductDescriptors() {
		t.Run(product.ID, func(t *testing.T) {
			handler := newRemoteLaneTestHandler()
			dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
				LocalHostID: "host-target", Handler: handler,
			})
			if err != nil {
				t.Fatal(err)
			}
			envelope := remoteLaneTestEnvelope(product.ID, "direct-"+product.ID)
			accepted, err := dispatcher.Dispatch(context.Background(), envelope)
			if err != nil {
				t.Fatalf("dispatch remote lane: %v", err)
			}
			if handler.callCount(envelope.RequestID) != 1 {
				t.Fatalf("destination handler calls = %d, want one", handler.callCount(envelope.RequestID))
			}
			request, ok := handler.request(envelope.RequestID)
			if !ok {
				t.Fatal("direct destination handler received no request")
			}
			if request.RequestID != envelope.RequestID || request.TargetHostID != "host-target" ||
				request.Product != product.ID || request.LaneSessionID != envelope.LaneSessionID ||
				request.TurnID != envelope.TurnID || !reflect.DeepEqual(request.InputReference, envelope.InputReference) {
				t.Fatalf("direct request = %#v, envelope=%#v", request, envelope)
			}
			if accepted.RequestID != envelope.RequestID || accepted.LaneSessionID != envelope.LaneSessionID ||
				accepted.TurnID != envelope.TurnID || accepted.AcceptedRevision == 0 {
				t.Fatalf("accepted remote lane = %#v", accepted)
			}
		})
	}
}

func TestRemoteLaneDispatchPropagatesExactAttestedParentGroupsAndPermission(t *testing.T) {
	handler := newRemoteLaneTestHandler()
	dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
		LocalHostID: "host-target", Handler: handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := remoteLaneTestEnvelope("qwen", "context")
	envelope.Parent = Peer{
		ID: "host-source/parent", HostID: "host-source", SessionID: "parent",
		Entrypoint: "claude", InstanceID: "claude-profile-a", PermissionMode: "bypassPermissions",
		Groups: []string{"zeta", "session:host-source/parent", "project", "project"},
	}
	envelope.SourceID = envelope.Parent.ID
	envelope.Groups = []string{"child-explicit", "child-explicit"}
	envelope.InheritParentGroups = true
	envelope.AllowDuplicateName = true
	envelope.PermissionMode = "default"
	if _, err := dispatcher.Dispatch(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	request, ok := handler.request(envelope.RequestID)
	if !ok {
		t.Fatal("destination handler received no exact context")
	}
	if request.ParentHostID != envelope.Parent.HostID || request.ParentSessionID != envelope.Parent.SessionID ||
		request.ParentProduct != envelope.Parent.Entrypoint || request.ParentInstanceID != envelope.Parent.InstanceID ||
		request.ParentPermissionMode != envelope.Parent.PermissionMode {
		t.Fatalf("parent identity/permission = %#v, parent=%#v", request, envelope.Parent)
	}
	wantParentGroups := []string{"project", "session:host-source/parent", "zeta"}
	wantExplicitGroups := []string{"child-explicit"}
	if !reflect.DeepEqual(request.ParentGroups, wantParentGroups) ||
		!reflect.DeepEqual(request.Groups, wantExplicitGroups) || !request.InheritParentGroups ||
		!request.AllowDuplicateName || request.PermissionMode != "default" {
		t.Fatalf("remote group/permission context = %#v", request)
	}

	tampered := remoteLaneTestEnvelope("qwen", "tampered")
	tampered.SourceID = "host-source/someone-else"
	if _, err := dispatcher.Dispatch(context.Background(), tampered); !errors.Is(err, ErrRemoteLaneParentMismatch) {
		t.Fatalf("tampered source error = %v, want ErrRemoteLaneParentMismatch", err)
	}
	if handler.callCount(tampered.RequestID) != 0 {
		t.Fatal("tampered parent context reached the destination lane authority")
	}
}

func TestRemoteLaneDispatchAcceptsProductScopedNativePermissions(t *testing.T) {
	tests := []struct {
		product string
		modes   []string
	}{
		{product: "codex", modes: []string{"default", "bypassPermissions"}},
		{product: "claude", modes: []string{"default", "acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"}},
		{product: "grok", modes: []string{"default", "bypassPermissions"}},
		{product: "qwen", modes: []string{"default", "bypassPermissions", "yolo", "plan", "auto", "accept_edits"}},
	}
	for _, test := range tests {
		for _, mode := range test.modes {
			t.Run(test.product+"/"+mode, func(t *testing.T) {
				handler := newRemoteLaneTestHandler()
				dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
					LocalHostID: "host-target", Handler: handler,
				})
				if err != nil {
					t.Fatal(err)
				}
				envelope := remoteLaneTestEnvelope(test.product, test.product+"-"+mode)
				envelope.PermissionMode = mode
				if _, err := dispatcher.Dispatch(context.Background(), envelope); err != nil {
					t.Fatalf("dispatch %s permission %q: %v", test.product, mode, err)
				}
			})
		}
	}

	handler := newRemoteLaneTestHandler()
	dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
		LocalHostID: "host-target", Handler: handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	invalid := remoteLaneTestEnvelope("codex", "cross-product-permission")
	invalid.PermissionMode = "plan"
	if _, err := dispatcher.Dispatch(context.Background(), invalid); err == nil {
		t.Fatal("Codex accepted a Claude/Qwen-only permission mode")
	}
}

func TestRemoteLaneDispatchIsIdempotentWithoutDuplicateDestinationWork(t *testing.T) {
	handler := newRemoteLaneTestHandler()
	dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
		LocalHostID: "host-target", Handler: handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := remoteLaneTestEnvelope("grok", "idempotent")
	first, err := dispatcher.Dispatch(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatcher.Dispatch(context.Background(), envelope)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent result changed: first=%#v second=%#v", first, second)
	}
	if got := handler.callCount(envelope.RequestID); got != 1 {
		t.Fatalf("duplicate destination dispatch count = %d, want one", got)
	}

	changed := envelope
	changed.InputReference = map[string]any{"content_ref": "different-input"}
	if _, err := dispatcher.Dispatch(context.Background(), changed); !errors.Is(err, ErrRemoteLaneIdempotencyConflict) {
		t.Fatalf("changed duplicate error = %v, want ErrRemoteLaneIdempotencyConflict", err)
	}
	if got := handler.callCount(envelope.RequestID); got != 1 {
		t.Fatalf("idempotency conflict reached destination handler %d times", got)
	}
}

func TestRemoteLaneResultPublishesOneContentFreeParentNotice(t *testing.T) {
	const resultCanary = "REMOTE_LANE_RESULT_MUST_NOT_ENTER_NOTICE_4b761d"
	handler := newRemoteLaneTestHandler()
	var (
		noticeMu sync.Mutex
		notices  []RemoteLaneNotice
	)
	dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
		LocalHostID: "host-target", Handler: handler,
		PublishNotice: func(_ context.Context, notice RemoteLaneNotice) error {
			noticeMu.Lock()
			defer noticeMu.Unlock()
			notices = append(notices, notice)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := remoteLaneTestEnvelope("codex", "notice")
	accepted, err := dispatcher.Dispatch(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	result := RemoteLaneResult{
		RequestID: accepted.RequestID, LaneSessionID: accepted.LaneSessionID, TurnID: accepted.TurnID,
		Outcome: "completed", ResultReference: map[string]any{"content": resultCanary, "native_result_id": "result-1"},
	}
	first, err := dispatcher.PublishResult(context.Background(), result)
	if err != nil {
		t.Fatalf("publish terminal result: %v", err)
	}
	second, err := dispatcher.PublishResult(context.Background(), result)
	if err != nil {
		t.Fatalf("replay terminal result: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("terminal notice replay changed: first=%#v second=%#v", first, second)
	}
	noticeMu.Lock()
	published := append([]RemoteLaneNotice(nil), notices...)
	noticeMu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published notices = %#v, want exactly one", published)
	}
	if first.TargetHostID != envelope.Parent.HostID || first.TargetSessionID != envelope.Parent.SessionID ||
		first.LaneSessionID != accepted.LaneSessionID || first.TurnID != accepted.TurnID ||
		first.Outcome != result.Outcome || first.NoticeID == "" {
		t.Fatalf("terminal notice pointer = %#v", first)
	}
	body, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), resultCanary) {
		t.Fatalf("terminal notice leaked lane result content: %s", body)
	}

	changed := result
	changed.Outcome = "failed"
	if _, err := dispatcher.PublishResult(context.Background(), changed); !errors.Is(err, ErrRemoteLaneIdempotencyConflict) {
		t.Fatalf("changed terminal replay = %v, want ErrRemoteLaneIdempotencyConflict", err)
	}
	noticeMu.Lock()
	noticeCount := len(notices)
	noticeMu.Unlock()
	if noticeCount != 1 {
		t.Fatal("conflicting terminal replay published a second notice")
	}
}

func TestRemoteLaneDispatcherRetriesTerminalOutboxAfterAcknowledgementFailure(t *testing.T) {
	handler := newRemoteLaneTestHandler()
	var (
		mu          sync.Mutex
		resultCalls int
		noticeCalls int
	)
	dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
		LocalHostID: "host-target",
		Handler:     handler,
		PublishResult: func(_ context.Context, result RemoteLaneResult) error {
			mu.Lock()
			defer mu.Unlock()
			resultCalls++
			if resultCalls == 1 {
				return errors.New("source acknowledgement lost")
			}
			if result.ResultReference["native_result_id"] != "result-retry" {
				t.Fatalf("retried result evidence = %#v", result.ResultReference)
			}
			return nil
		},
		PublishNotice: func(_ context.Context, notice RemoteLaneNotice) error {
			mu.Lock()
			defer mu.Unlock()
			noticeCalls++
			if notice.RequestID != "request-outbox" {
				t.Fatalf("retried notice = %#v", notice)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := remoteLaneTestEnvelope("qwen", "outbox")
	accepted, err := dispatcher.Dispatch(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	result := RemoteLaneResult{
		RequestID: accepted.RequestID, LaneSessionID: accepted.LaneSessionID, TurnID: accepted.TurnID,
		Outcome: "completed", ResultReference: map[string]any{"native_result_id": "result-retry"},
	}
	if _, err := dispatcher.PublishResult(context.Background(), result); err == nil || !strings.Contains(err.Error(), "acknowledgement lost") {
		t.Fatalf("first outbox publication error = %v", err)
	}
	first, err := dispatcher.PublishResult(context.Background(), result)
	if err != nil {
		t.Fatalf("retry terminal outbox: %v", err)
	}
	second, err := dispatcher.PublishResult(context.Background(), result)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent terminal outbox replay = %#v, %v", second, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if resultCalls != 2 || noticeCalls != 1 {
		t.Fatalf("terminal publisher calls result=%d notice=%d, want 2/1", resultCalls, noticeCalls)
	}
}

func TestRemoteLaneDispatchHasNoLaneWatchProcessBoundary(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "lane-watch-started")
	watch := filepath.Join(root, "lane-watch")
	script := "#!/bin/sh\nprintf started >\"$REMOTE_LANE_WATCH_MARKER\"\n"
	if err := os.WriteFile(watch, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REMOTE_LANE_WATCH_MARKER", marker)

	handler := newRemoteLaneTestHandler()
	dispatcher, err := NewRemoteLaneDispatcher(RemoteLaneDispatcherOptions{
		LocalHostID: "host-target", Handler: handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := remoteLaneTestEnvelope("claude", "no-watch")
	if _, err := dispatcher.Dispatch(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("remote dispatch started a lane-watch process: %v", err)
	}
	if handler.callCount(envelope.RequestID) != 1 {
		t.Fatal("no-watch dispatch did not call the in-process handler")
	}

	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve federation test path")
	}
	implementation := filepath.Join(filepath.Dir(current), "lane.go")
	body, err := os.ReadFile(implementation)
	if err != nil {
		t.Fatalf("read logical remote lane implementation: %v", err)
	}
	for _, forbidden := range []string{"os/exec", "exec.Command", "lane-watch", "lane_watch"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("logical federation lane implementation retains process boundary %q", forbidden)
		}
	}
	legacyWatch := filepath.Join(filepath.Dir(current), "..", "federator", "lane_watch.go")
	if _, err := os.Stat(legacyWatch); !os.IsNotExist(err) {
		t.Fatalf("legacy lane-watch implementation remains at %s", legacyWatch)
	}
}

type remoteLaneTestHandler struct {
	mu       sync.Mutex
	requests map[string][]RemoteLaneRequest
}

func newRemoteLaneTestHandler() *remoteLaneTestHandler {
	return &remoteLaneTestHandler{requests: make(map[string][]RemoteLaneRequest)}
}

func (handler *remoteLaneTestHandler) StartRemoteLane(
	_ context.Context,
	request RemoteLaneRequest,
) (RemoteLaneAccepted, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.requests[request.RequestID] = append(handler.requests[request.RequestID], cloneRemoteLaneTestRequest(request))
	return RemoteLaneAccepted{
		RequestID: request.RequestID, LaneSessionID: request.LaneSessionID, TurnID: request.TurnID,
		AcceptedRevision: 1,
	}, nil
}

func (handler *remoteLaneTestHandler) callCount(requestID string) int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return len(handler.requests[requestID])
}

func (handler *remoteLaneTestHandler) request(requestID string) (RemoteLaneRequest, bool) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	requests := handler.requests[requestID]
	if len(requests) == 0 {
		return RemoteLaneRequest{}, false
	}
	return cloneRemoteLaneTestRequest(requests[0]), true
}

func remoteLaneTestEnvelope(product, suffix string) RemoteLaneEnvelope {
	parent := Peer{
		ID: "host-source/parent-" + suffix, HostID: "host-source", SessionID: "parent-" + suffix,
		Entrypoint: "codex", InstanceID: "codex-profile", PermissionMode: "default",
		Groups: []string{"project", "session:host-source/parent-" + suffix},
	}
	return RemoteLaneEnvelope{
		RequestID: "request-" + suffix, SourceID: parent.ID, TargetHostID: "host-target", Parent: parent,
		Product: product, LaneSessionID: "lane-" + suffix, TurnID: "turn-" + suffix,
		Name: "worker-" + suffix, Cwd: "/workspace", Groups: []string{"child-explicit"},
		InheritParentGroups: true, PermissionMode: "default",
		InputReference: map[string]any{"content_ref": "input-" + suffix},
	}
}

func cloneRemoteLaneTestRequest(request RemoteLaneRequest) RemoteLaneRequest {
	request.ParentGroups = append([]string(nil), request.ParentGroups...)
	request.Groups = append([]string(nil), request.Groups...)
	request.InputReference = cloneRemoteLaneTestMap(request.InputReference)
	return request
}

func cloneRemoteLaneTestMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
