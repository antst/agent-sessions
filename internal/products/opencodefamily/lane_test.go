package opencodefamily

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type testReceipts struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	corrupt bool
}

func (receipts *testReceipts) OpenReceipt(id string) (io.ReadCloser, int64, [32]byte, error) {
	receipts.mu.Lock()
	defer receipts.mu.Unlock()
	body, ok := receipts.bodies[id]
	if !ok {
		return nil, 0, [32]byte{}, errors.New("missing receipt")
	}
	digest := sha256.Sum256(body)
	if receipts.corrupt {
		digest[0] ^= 0xff
	}
	copyBody := append([]byte(nil), body...)
	return io.NopCloser(bytes.NewReader(copyBody)), int64(len(copyBody)), digest, nil
}

type testServerManager struct {
	client       *Client
	openCount    atomic.Int64
	recoverCount atomic.Int64
	closed       atomic.Int64
}

func (manager *testServerManager) live() *LiveServer {
	return &LiveServer{client: manager.client, closeFn: func(context.Context) error { manager.closed.Add(1); return nil }}
}

func (manager *testServerManager) Open(context.Context, ServerOpenRequest) (*LiveServer, error) {
	manager.openCount.Add(1)
	return manager.live(), nil
}

func (manager *testServerManager) Recover(context.Context, ServerRecoveryRequest) (*LiveServer, error) {
	manager.recoverCount.Add(1)
	return manager.live(), nil
}

func TestOpenCodeLaneLifecycleUsesReceiptAndExactRecovery(t *testing.T) {
	var messageID atomic.Value
	messageID.Store("")
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			_, _ = response.Write([]byte(`{"id":"ses_lane","title":""}`))
		case "GET /session/ses_lane":
			_, _ = response.Write([]byte(`{"id":"ses_lane","title":""}`))
		case "POST /session/ses_lane/prompt_async":
			var body struct {
				ID string `json:"messageID"`
			}
			if decodeJSON(request.Body, &body) != nil || body.ID == "" {
				t.Errorf("prompt body = %#v", body)
			}
			messageID.Store(body.ID)
			response.WriteHeader(http.StatusNoContent)
		case "GET /event":
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(response, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_lane\"}}\n\n")
		case "GET /session/ses_lane/message":
			_, _ = fmt.Fprintf(response, `[{"info":{"id":%q,"sessionID":"ses_lane","role":"user"},"parts":[]},{"info":{"id":"msg_answer","sessionID":"ses_lane","role":"assistant","parentID":%q,"time":{"completed":123}},"parts":[{"type":"text","text":"lane result"}]}]`, messageID.Load().(string), messageID.Load().(string))
		case "POST /session/ses_lane/abort":
			_, _ = response.Write([]byte("true"))
		case "DELETE /session/ses_lane":
			_, _ = response.Write([]byte("true"))
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	receipts := &testReceipts{bodies: map[string][]byte{"receipt-one": []byte("perform task")}}
	servers := &testServerManager{client: client}
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 7,
		Receipts: receipts, Servers: servers, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "lane-one", Cwd: "/work/project", PermissionMode: permissionmode.Default,
	})
	if err != nil || session.NativeSessionID != "ses_lane" || session.Generation != 7 {
		t.Fatalf("open = %#v, %v", session, err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "receipt-one", PermissionMode: permissionmode.Default})
	if err != nil || turn.NativeTurnID == "" {
		t.Fatalf("start = %#v, %v", turn, err)
	}
	if _, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "receipt-one", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrUnsupportedSteer) {
		t.Fatalf("OpenCode steer = %v", err)
	}
	terminal, err := driver.WaitTurn(context.Background(), turn)
	if err != nil || terminal.Outcome != productruntime.TurnCompleted || terminal.Result != "lane result" || terminal.ResultDigest != sha256.Sum256([]byte("lane result")) {
		t.Fatalf("terminal = %#v, %v", terminal, err)
	}
	if err := driver.Archive(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := driver.Archive(context.Background(), session); err != nil {
		t.Fatalf("idempotent archive = %v", err)
	}
	if servers.closed.Load() != 1 {
		t.Fatalf("server closes = %d", servers.closed.Load())
	}

	unsupportedRecovery, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 8,
		Receipts: receipts, Servers: servers, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unsupportedRecovery.Recover(context.Background(), productruntime.LaneRecoveryRequest{
		ProductID: "opencode", LaneID: "lane-one", PriorNativeSessionID: "ses_lane", PriorGeneration: 7,
	}); !errors.Is(err, productruntime.ErrUnsupportedRecovery) || servers.recoverCount.Load() != 0 {
		t.Fatalf("recovery without durable permission mode = %v, calls=%d", err, servers.recoverCount.Load())
	}

	recoveredDriver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 8,
		Receipts: receipts, Servers: servers, MapPermission: MapPermissionRules,
		RecoveryMode: func(context.Context, productruntime.LaneRecoveryRequest) (permissionmode.Mode, error) {
			return permissionmode.Default, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoveredDriver.Recover(context.Background(), productruntime.LaneRecoveryRequest{
		ProductID: "opencode", LaneID: "lane-one", PriorNativeSessionID: "ses_lane", PriorGeneration: 7,
	})
	if err != nil || recovered.NativeSessionID != "ses_lane" || recovered.Generation != 8 || servers.recoverCount.Load() != 1 {
		t.Fatalf("recover = %#v, %v, calls=%d", recovered, err, servers.recoverCount.Load())
	}
}

func TestKiloLaneSteerUsesExplicitV2Route(t *testing.T) {
	var deliveriesMu sync.Mutex
	deliveries := []string{}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuthOnly(t, request)
		if strings.HasPrefix(request.URL.Path, "/api/session/") {
			if request.URL.RawQuery != "" {
				t.Errorf("Kilo v2 request query = %q", request.URL.RawQuery)
			}
		} else if request.URL.Query().Get("directory") != "/work/project" {
			t.Errorf("directory = %q", request.URL.Query().Get("directory"))
		}
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			_, _ = response.Write([]byte(`{"id":"ses_kilo_lane","title":""}`))
		case "POST /api/session/ses_kilo_lane/prompt":
			var body struct {
				ID       string `json:"id"`
				Delivery string `json:"delivery"`
			}
			if decodeJSON(request.Body, &body) != nil {
				t.Error("invalid prompt")
			}
			deliveriesMu.Lock()
			deliveries = append(deliveries, body.Delivery)
			deliveriesMu.Unlock()
			_, _ = fmt.Fprintf(response, `{"data":{"id":%q,"sessionID":"ses_kilo_lane","delivery":%q}}`, body.ID, body.Delivery)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectKilo, handler)
	defer closeClient()
	receipts := &testReceipts{bodies: map[string][]byte{"initial": []byte("first"), "busy": []byte("second")}}
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "kilo", Dialect: DialectKilo, Generation: 3,
		Receipts: receipts, Servers: &testServerManager{client: client}, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "kilo", LaneID: "lane-kilo", Cwd: "/work/project", PermissionMode: permissionmode.BypassPermissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "initial", PermissionMode: permissionmode.BypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "busy", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("changed Kilo permission mode = %v", err)
	}
	accepted, err := driver.Steer(context.Background(), turn, productruntime.TurnStartRequest{ReceiptID: "busy", PermissionMode: permissionmode.BypassPermissions})
	if err != nil || accepted.NativeSessionID != "ses_kilo_lane" || accepted.NativeMessageID == turn.NativeTurnID {
		t.Fatalf("steer = %#v, %v", accepted, err)
	}
	deliveriesMu.Lock()
	got := append([]string(nil), deliveries...)
	deliveriesMu.Unlock()
	if fmt.Sprint(got) != "[queue steer]" {
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
			_, _ = fmt.Fprintf(response, `[{"info":{"id":%q,"sessionID":"ses_fast","role":"user"},"parts":[]},{"info":{"id":"msg_fast_answer","sessionID":"ses_fast","role":"assistant","parentID":%q,"time":{"completed":999}},"parts":[{"type":"text","text":"fast result"}]}]`, id, id)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 1,
		Receipts: &testReceipts{bodies: map[string][]byte{"fast": []byte("finish immediately")}},
		Servers:  &testServerManager{client: client}, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "fast", Cwd: "/work/project", PermissionMode: permissionmode.Default,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "fast", PermissionMode: permissionmode.Default})
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

func TestLaneRejectsChangedReceiptAndConcurrentTurn(t *testing.T) {
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
	receipts := &testReceipts{bodies: map[string][]byte{"one": []byte("one"), "two": []byte("two")}, corrupt: true}
	driver, _ := NewLaneDriver(LaneConfig{ProductID: "opencode", Dialect: DialectOpenCode, Generation: 1, Receipts: receipts, Servers: &testServerManager{client: client}, MapPermission: MapPermissionRules})
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: "opencode", LaneID: "race", Cwd: "/work/project", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "one", PermissionMode: permissionmode.BypassPermissions}); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("changed lane permission mode = %v", err)
	}
	if promptCalls.Load() != 0 {
		t.Fatal("changed permission mode reached native I/O")
	}
	if _, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "one", PermissionMode: permissionmode.Default}); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("changed receipt = %v", err)
	}
	if promptCalls.Load() != 0 {
		t.Fatal("changed receipt reached native I/O")
	}
	receipts.mu.Lock()
	receipts.corrupt = false
	receipts.mu.Unlock()

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, receiptID := range []string{"one", "two"} {
		go func(id string) {
			<-start
			_, startErr := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: id, PermissionMode: permissionmode.Default})
			results <- startErr
		}(receiptID)
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
		Receipts: &testReceipts{bodies: map[string][]byte{"interrupt": []byte("slow work")}},
		Servers:  &testServerManager{client: client}, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := driver.Open(context.Background(), productruntime.LaneOpenRequest{ProductID: "opencode", LaneID: "interrupt-lane", Cwd: "/work/project", PermissionMode: permissionmode.Default})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := driver.StartTurn(context.Background(), session, productruntime.TurnStartRequest{ReceiptID: "interrupt", PermissionMode: permissionmode.Default})
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
