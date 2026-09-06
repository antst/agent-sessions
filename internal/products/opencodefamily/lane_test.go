package opencodefamily

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/permissionmode"
	"github.com/antst/sessionbus/internal/productruntime"
)

type testServerManager struct {
	client    *Client
	openCount atomic.Int64
	closed    atomic.Int64
	request   atomic.Value
}

func (manager *testServerManager) live() *LiveServer {
	return &LiveServer{client: manager.client, closeFn: func(context.Context) error { manager.closed.Add(1); return nil }}
}

func (manager *testServerManager) Open(_ context.Context, request ServerOpenRequest) (*LiveServer, error) {
	manager.openCount.Add(1)
	manager.request.Store(request)
	return manager.live(), nil
}

func TestOpenCodeLaneLifecycleUsesDirectPrompt(t *testing.T) {
	var messageID atomic.Value
	messageID.Store("")
	var getCalls atomic.Int64
	var deleteCalls atomic.Int64
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			var body struct {
				Title string `json:"title"`
			}
			if decodeJSON(request.Body, &body) != nil || body.Title != "worker" {
				t.Errorf("create title = %q", body.Title)
			}
			_, _ = response.Write([]byte(`{"id":"ses_lane","title":"worker"}`))
		case "GET /session/ses_lane":
			getCalls.Add(1)
			_, _ = response.Write([]byte(`{"id":"ses_lane","title":"worker"}`))
		case "POST /session/ses_lane/prompt_async":
			var body struct {
				ID      string       `json:"messageID"`
				Model   *NativeModel `json:"model"`
				Agent   string       `json:"agent"`
				Variant string       `json:"variant"`
			}
			if decodeJSON(request.Body, &body) != nil || body.ID == "" || body.Model == nil ||
				body.Model.ProviderID != "google" || body.Model.ModelID != "gemini-3.1-pro-preview" ||
				body.Agent != "octto" || body.Variant != "high" {
				t.Errorf("prompt body = %#v", body)
			}
			messageID.Store(body.ID)
			response.WriteHeader(http.StatusNoContent)
		case "GET /event":
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(response, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_lane\"}}\n\n")
		case "GET /session/ses_lane/message":
			_, _ = fmt.Fprintf(response, `[{"info":{"id":%q,"sessionID":"ses_lane","role":"user"},"parts":[]},{"info":{"id":"msg_answer","sessionID":"ses_lane","role":"assistant","parentID":%q,"finish":"stop","time":{"completed":123}},"parts":[{"type":"text","text":"lane result"}]}]`, messageID.Load().(string), messageID.Load().(string))
		case "POST /session/ses_lane/abort":
			_, _ = response.Write([]byte("true"))
		case "DELETE /session/ses_lane":
			deleteCalls.Add(1)
			_, _ = response.Write([]byte("true"))
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	servers := &testServerManager{client: client}
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 7,
		Servers: servers, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "lane-one", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.Default,
		Arguments:   []string{"--model", "google/gemini-3.1-pro-preview", "--agent", "octto"},
		Environment: []string{"AGENT_SESSIONS_PRODUCT=opencode", "AGENT_SESSIONS_SESSION_ID=lane-one", "AGENT_SESSIONS_GROUPS=[]"},
	})
	if err != nil || session.LaneID != "ses_lane" || session.NativeSessionID != "ses_lane" || session.Generation != 7 {
		t.Fatalf("open = %#v, %v", session, err)
	}
	split := session
	split.LaneID = "lane-one"
	if _, err := driver.StartTurn(context.Background(), split, productruntime.TurnStartRequest{Prompt: "must not run", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("provisional identity start error = %v", err)
	}
	if got := servers.request.Load().(ServerOpenRequest).Arguments; len(got) != 0 {
		t.Fatalf("serve arguments = %v, want model consumed by prompt", got)
	}
	wantEnvironment := []productruntime.EnvVar{
		{Name: "AGENT_SESSIONS_PRODUCT", Value: "opencode"},
		{Name: "AGENT_SESSIONS_SESSION_ID", Value: "lane-one"},
		{Name: "AGENT_SESSIONS_GROUPS", Value: "[]"},
	}
	if got := servers.request.Load().(ServerOpenRequest).Env; !reflect.DeepEqual(got, wantEnvironment) {
		t.Fatalf("serve environment = %#v, want %#v", got, wantEnvironment)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{
		Prompt: "perform task", PermissionMode: permissionmode.Default, Effort: "high",
		Arguments: []string{"--model", "google/gemini-3.1-pro-preview", "--agent", "octto"},
	})
	if err != nil || turn.NativeTurnID == "" {
		t.Fatalf("start = %#v, %v", turn, err)
	}
	if _, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{Prompt: "perform task", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrUnsupportedSteer) {
		t.Fatalf("OpenCode steer = %v", err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "lane result" || terminal.ResultDigest != sha256.Sum256([]byte("lane result")) {
		t.Fatalf("terminal = %#v, %v", terminal, err)
	}
	reused, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: session.NativeSessionID, ResumeNativeID: session.NativeSessionID,
		Cwd: "/work/project", PermissionMode: permissionmode.Default,
	})
	if err != nil || reused != session || servers.openCount.Load() != 1 {
		t.Fatalf("reuse = %#v, %v; server opens = %d", reused, err, servers.openCount.Load())
	}
	if err := driver.Archive(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if deleteCalls.Load() != 0 {
		t.Fatalf("archive deleted product session %d times", deleteCalls.Load())
	}
	if servers.closed.Load() != 1 {
		t.Fatalf("server closes = %d", servers.closed.Load())
	}
	resumed, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: session.NativeSessionID, ResumeNativeID: session.NativeSessionID,
		Cwd: "/work/project", PermissionMode: permissionmode.Default,
	})
	if err != nil || resumed != session || getCalls.Load() != 1 || servers.openCount.Load() != 2 {
		t.Fatalf("resume = %#v, %v; gets=%d opens=%d", resumed, err, getCalls.Load(), servers.openCount.Load())
	}
}

func TestFamilyLaneRejectsMalformedModelBeforeStartingServer(t *testing.T) {
	for _, dialect := range []Dialect{DialectOpenCode, DialectKilo} {
		t.Run(string(dialect), func(t *testing.T) {
			servers := &testServerManager{}
			driver, err := NewLaneDriver(LaneConfig{
				ProductID: string(dialect), Dialect: dialect, Generation: 7,
				Servers: servers, MapPermission: MapPermissionRules,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
				ProductID: string(dialect), LaneID: "lane-one", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.Default,
				Arguments: []string{"--model", "missing-provider-separator"},
			})
			if err == nil || servers.openCount.Load() != 0 {
				t.Fatalf("malformed model error = %v; server opens = %d", err, servers.openCount.Load())
			}
		})
	}
}

func TestKiloLaneInitialPromptUsesLegacyRouteAndRejectsSteer(t *testing.T) {
	var deliveriesMu sync.Mutex
	deliveries := []string{}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuthOnly(t, request)
		if request.URL.Query().Get("directory") != "/work/project" {
			t.Errorf("directory = %q", request.URL.Query().Get("directory"))
		}
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			_, _ = response.Write([]byte(`{"id":"ses_kilo_lane","title":""}`))
		case "POST /session/ses_kilo_lane/prompt_async":
			var body struct {
				ID      string       `json:"messageID"`
				Model   *NativeModel `json:"model"`
				Agent   string       `json:"agent"`
				Variant string       `json:"variant"`
			}
			if decodeJSON(request.Body, &body) != nil || body.ID == "" || body.Model == nil ||
				body.Model.ProviderID != "deepseek" || body.Model.ModelID != "deepseek-v4-flash" ||
				body.Agent != "plan" || body.Variant != "minimal" {
				t.Errorf("Kilo initial prompt = %#v", body)
			}
			deliveriesMu.Lock()
			deliveries = append(deliveries, "prompt_async")
			deliveriesMu.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectKilo, handler)
	defer closeClient()
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "kilo", Dialect: DialectKilo, Generation: 3,
		Servers: &testServerManager{client: client}, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "kilo", LaneID: "lane-kilo", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.BypassPermissions,
		Arguments:   []string{"--model", "deepseek/deepseek-v4-flash", "--agent", "plan"},
		Environment: []string{"AGENT_SESSIONS_PRODUCT=kilo", "AGENT_SESSIONS_SESSION_ID=lane-kilo", "AGENT_SESSIONS_GROUPS=[]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := driver.config.Servers.(*testServerManager).request.Load().(ServerOpenRequest).Arguments; len(got) != 0 {
		t.Fatalf("Kilo serve arguments = %v, want model consumed by prompt", got)
	}
	wantEnvironment := []productruntime.EnvVar{
		{Name: "AGENT_SESSIONS_PRODUCT", Value: "kilo"},
		{Name: "AGENT_SESSIONS_SESSION_ID", Value: "lane-kilo"},
		{Name: "AGENT_SESSIONS_GROUPS", Value: "[]"},
	}
	if got := driver.config.Servers.(*testServerManager).request.Load().(ServerOpenRequest).Env; !reflect.DeepEqual(got, wantEnvironment) {
		t.Fatalf("Kilo serve environment = %#v, want %#v", got, wantEnvironment)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{
		Prompt: "first", PermissionMode: permissionmode.BypassPermissions, Effort: "minimal",
		Arguments: []string{"--model", "deepseek/deepseek-v4-flash", "--agent", "plan"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if driver.Capabilities().Steer {
		t.Fatal("Kilo advertised a steer operation the product accepts but ignores")
	}
	if _, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{Prompt: "second", PermissionMode: permissionmode.BypassPermissions}); !errors.Is(err, productruntime.ErrUnsupportedSteer) {
		t.Fatalf("Kilo steer error = %v", err)
	}
	deliveriesMu.Lock()
	got := append([]string(nil), deliveries...)
	deliveriesMu.Unlock()
	if fmt.Sprint(got) != "[prompt_async]" {
		t.Fatalf("Kilo delivery routes = %v", got)
	}
}

func TestLaneReconcilesCompletedMessageWhenFastTerminalEventWasMissed(t *testing.T) {
	var nativeMessageID atomic.Value
	nativeMessageID.Store("")
	var eventSubscriptions atomic.Int64
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			_, _ = response.Write([]byte(`{"id":"ses_fast"}`))
		case "POST /session/ses_fast/prompt_async":
			var body struct {
				ID string `json:"messageID"`
			}
			if decodeJSON(request.Body, &body) != nil || body.ID == "" {
				t.Fatalf("prompt body = %#v", body)
			}
			nativeMessageID.Store(body.ID)
			response.WriteHeader(http.StatusNoContent)
		case "GET /event":
			// The terminal occurred before this subscription. A 204 stream close
			// must not strand WaitTurn.
			eventSubscriptions.Add(1)
			response.WriteHeader(http.StatusNoContent)
		case "GET /session/ses_fast/message":
			id := nativeMessageID.Load().(string)
			_, _ = fmt.Fprintf(response, `[{"info":{"id":%q,"sessionID":"ses_fast","role":"user"},"parts":[]},{"info":{"id":"msg_synthetic","sessionID":"ses_fast","role":"user"},"parts":[]},{"info":{"id":"msg_fast_answer","sessionID":"ses_fast","role":"assistant","parentID":"msg_synthetic","finish":"stop","time":{"completed":999}},"parts":[{"type":"text","text":"fast result"}]}]`, id)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 1,
		Servers: &testServerManager{client: client}, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "fast", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{Prompt: "finish immediately", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	terminal, err := driver.WaitTurn(ctx, turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "fast result" {
		t.Fatalf("reconciled terminal = %#v, %v", terminal, err)
	}
	if eventSubscriptions.Load() > 1 {
		t.Fatalf("event subscriptions = %d", eventSubscriptions.Load())
	}
}

func TestLaneReturnsNativeSessionErrorWithFailedTerminal(t *testing.T) {
	var nativeMessageID atomic.Value
	nativeMessageID.Store("")
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			_, _ = response.Write([]byte(`{"id":"ses_error"}`))
		case "POST /session/ses_error/prompt_async":
			var body struct {
				ID string `json:"messageID"`
			}
			if decodeJSON(request.Body, &body) != nil || body.ID == "" {
				t.Fatalf("prompt body = %#v", body)
			}
			nativeMessageID.Store(body.ID)
			response.WriteHeader(http.StatusNoContent)
		case "GET /session/ses_error/message":
			_, _ = fmt.Fprintf(response, `[{"info":{"id":%q,"sessionID":"ses_error","role":"user"},"parts":[]}]`, nativeMessageID.Load().(string))
		case "GET /event":
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(response, `data: {"type":"session.error","properties":{"sessionID":"ses_error","error":{"data":{"message":"Model not found: native/product-model"}}}}`+"\n\n")
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 1,
		Servers: &testServerManager{client: client}, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "error", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{Prompt: "fail", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err == nil || err.Error() != "Model not found: native/product-model" || terminal.Outcome != productruntime.TurnFailed || terminal.ExitLike != 1 {
		t.Fatalf("failed terminal = %#v, %v", terminal, err)
	}
}

func TestLaneRejectsChangedPermissionAndConcurrentTurn(t *testing.T) {
	var promptCalls atomic.Int64
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.URL.Path {
		case "/session":
			_, _ = response.Write([]byte(`{"id":"ses_race"}`))
		case "/session/ses_race/prompt_async":
			promptCalls.Add(1)
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	driver, _ := NewLaneDriver(LaneConfig{ProductID: "opencode", Dialect: DialectOpenCode, Generation: 1, Servers: &testServerManager{client: client}, MapPermission: MapPermissionRules})
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: "opencode", LaneID: "race", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{Prompt: "one", PermissionMode: permissionmode.BypassPermissions}); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("changed lane permission mode = %v", err)
	}
	if promptCalls.Load() != 0 {
		t.Fatal("changed permission mode reached native I/O")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, prompt := range []string{"one", "two"} {
		go func(prompt string) {
			<-start
			_, startErr := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{Prompt: prompt, PermissionMode: permissionmode.Default})
			results <- startErr
		}(prompt)
	}
	close(start)
	errA, errB := <-results, <-results
	accepted := 0
	for _, startErr := range []error{errA, errB} {
		if startErr == nil {
			accepted++
		} else if !errors.Is(startErr, productruntime.ErrNativeRejected) {
			t.Fatalf("race error = %v", startErr)
		}
	}
	if accepted != 1 || promptCalls.Load() != 1 {
		t.Fatalf("accepted=%d native calls=%d", accepted, promptCalls.Load())
	}
}

func TestLaneInterruptRacingIdleReportsInterruptedExactlyOnce(t *testing.T) {
	eventConnected := make(chan struct{})
	interruptAccepted := make(chan struct{})
	var signalOnce sync.Once
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			_, _ = response.Write([]byte(`{"id":"ses_interrupt"}`))
		case "POST /session/ses_interrupt/prompt_async":
			response.WriteHeader(http.StatusNoContent)
		case "GET /session/ses_interrupt/message":
			_, _ = response.Write([]byte(`[{"info":{"id":"msg_pending","sessionID":"ses_interrupt","role":"user"},"parts":[]}]`))
		case "GET /event":
			response.Header().Set("Content-Type", "text/event-stream")
			response.WriteHeader(http.StatusOK)
			if flusher, ok := response.(http.Flusher); ok {
				flusher.Flush()
			}
			signalOnce.Do(func() { close(eventConnected) })
			<-interruptAccepted
			_, _ = fmt.Fprint(response, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_interrupt\"}}\n\n")
		case "POST /session/ses_interrupt/abort":
			_, _ = response.Write([]byte("true"))
			close(interruptAccepted)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 1,
		Servers: &testServerManager{client: client}, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: "opencode", LaneID: "interrupt-lane", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{Prompt: "slow work", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct {
		terminal productruntime.NativeTerminal
		err      error
	}, 1)
	go func() {
		terminal, waitErr := driver.WaitTurn(context.Background(), turn)
		waited <- struct {
			terminal productruntime.NativeTerminal
			err      error
		}{terminal: terminal, err: waitErr}
	}()
	select {
	case <-eventConnected:
	case <-time.After(time.Second):
		t.Fatal("WaitTurn did not establish event stream")
	}
	if err := driver.Interrupt(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	result := <-waited
	if result.err != nil || result.terminal.Outcome != productruntime.TurnInterrupted || result.terminal.ExitLike != 130 {
		t.Fatalf("interrupted terminal = %#v, %v", result.terminal, result.err)
	}
	if _, err := driver.WaitTurn(context.Background(), turn); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("consumed interrupt replay = %v", err)
	}
}

func decodeJSON(reader io.Reader, value any) error { return jsonNewDecoder(reader).Decode(value) }

// Kept behind a variable so hostile tests can prove decoding occurs exactly
// once without changing production behavior.
var jsonNewDecoder = func(reader io.Reader) *json.Decoder { return json.NewDecoder(reader) }
