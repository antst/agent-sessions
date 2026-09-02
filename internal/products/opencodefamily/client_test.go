package opencodefamily

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

func newFamilyTestClient(t *testing.T, dialect Dialect, handler http.Handler) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	secret := productruntime.NewSensitiveValue("test-password")
	auth, err := productserver.NewBasicAuth("agent-sessions", secret)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := productserver.NewClient(productserver.ClientConfig{Endpoint: server.URL, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{HTTP: raw, Directory: "/work/project", Dialect: dialect, Now: func() time.Time { return time.Unix(123, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	return client, func() { raw.CloseIdleConnections(); server.Close() }
}

func requireBasicAuthOnly(t *testing.T, request *http.Request) {
	t.Helper()
	username, password, ok := request.BasicAuth()
	if !ok || username != "agent-sessions" || password != "test-password" {
		t.Fatalf("native request auth = %q, %q, %v", username, password, ok)
	}
}

func requireBasicAuth(t *testing.T, request *http.Request) {
	t.Helper()
	requireBasicAuthOnly(t, request)
	if got := request.URL.Query().Get("directory"); got != "/work/project" {
		t.Fatalf("directory = %q", got)
	}
}

func TestOpenCodeTypedLifecycleAndPermissionRelay(t *testing.T) {
	var mu sync.Mutex
	permissionReplied := false
	messageID := ""
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "POST /session":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"id":"ses_exact","title":""}`))
		case "GET /session/ses_exact":
			_, _ = response.Write([]byte(`{"id":"ses_exact","title":"renamed"}`))
		case "PATCH /session/ses_exact":
			_, _ = response.Write([]byte(`{"id":"ses_exact","title":"renamed"}`))
		case "POST /session/ses_exact/prompt_async":
			var body struct {
				MessageID string          `json:"messageID"`
				NoReply   bool            `json:"noReply"`
				Model     json.RawMessage `json:"model"`
			}
			if json.NewDecoder(request.Body).Decode(&body) != nil || body.MessageID == "" || body.NoReply || len(body.Model) != 0 {
				t.Errorf("prompt body = %#v", body)
			}
			messageID = body.MessageID
			response.WriteHeader(http.StatusNoContent)
		case "GET /event":
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(response, "data: {\"type\":\"permission.asked\",\"properties\":{\"sessionID\":\"ses_exact\",\"id\":\"per_exact\"}}\n\n")
			_, _ = fmt.Fprint(response, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_exact\"}}\n\n")
		case "POST /session/ses_exact/permissions/per_exact":
			mu.Lock()
			permissionReplied = true
			mu.Unlock()
			_, _ = response.Write([]byte("true"))
		case "GET /session/ses_exact/message":
			_, _ = fmt.Fprintf(response, `[{"info":{"id":%q,"sessionID":"ses_exact","role":"user"},"parts":[{"type":"text","text":"input"}]},{"info":{"id":"msg_assistant","sessionID":"ses_exact","role":"assistant","parentID":%q,"time":{"completed":123}},"parts":[{"type":"text","text":"done"}]}]`, messageID, messageID)
		case "DELETE /session/ses_exact":
			_, _ = response.Write([]byte("true"))
		default:
			http.Error(response, "unexpected route", http.StatusNotFound)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()

	session, err := client.CreateSession(context.Background(), "", []PermissionRule{{Permission: "*", Pattern: "*", Action: "ask"}})
	if err != nil || session.ID != "ses_exact" {
		t.Fatalf("create = %#v, %v", session, err)
	}
	accepted, err := client.PromptAsync(context.Background(), session.ID, "receipt-1", []byte("input"), false, nil)
	if err != nil || accepted.NativeMessageID == "" || !accepted.AcceptedAt.Equal(time.Unix(123, 0)) {
		t.Fatalf("accepted = %#v, %v", accepted, err)
	}
	if err := client.WaitIdle(context.Background(), session.ID, func(context.Context, NativeEvent) (string, error) { return "once", nil }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	replied := permissionReplied
	mu.Unlock()
	if !replied {
		t.Fatal("permission event was not answered before idle")
	}
	result, err := client.ResultAfter(context.Background(), session.ID, accepted.NativeMessageID)
	if err != nil || result != "done" {
		t.Fatalf("result = %q, %v", result, err)
	}
	if err := client.RenameSession(context.Background(), session.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSession(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
}

func TestKiloLaneUsesV2PromptAndRejectsConflictingAcceptance(t *testing.T) {
	wrong := false
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuthOnly(t, request)
		if request.URL.RawQuery != "" {
			t.Errorf("Kilo v2 prompt query = %q", request.URL.RawQuery)
		}
		if request.URL.Path != "/api/session/ses_kilo/prompt" {
			http.NotFound(response, request)
			return
		}
		var body struct {
			ID       string `json:"id"`
			Delivery string `json:"delivery"`
			Prompt   struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil || body.Prompt.Text != "steer me" || body.Delivery != "steer" {
			t.Errorf("Kilo v2 prompt = %#v", body)
		}
		sessionID := "ses_kilo"
		if wrong {
			sessionID = "ses_foreign"
		}
		_, _ = fmt.Fprintf(response, `{"data":{"id":%q,"sessionID":%q,"delivery":"steer"}}`, body.ID, sessionID)
	})
	client, closeClient := newFamilyTestClient(t, DialectKilo, handler)
	defer closeClient()
	accepted, err := client.KiloPrompt(context.Background(), "ses_kilo", "receipt-steer", []byte("steer me"), "steer")
	if err != nil || accepted.NativeSessionID != "ses_kilo" {
		t.Fatalf("accepted = %#v, %v", accepted, err)
	}
	wrong = true
	if _, err := client.KiloPrompt(context.Background(), "ses_kilo", "receipt-foreign", []byte("steer me"), "steer"); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("conflicting native session acceptance = %v", err)
	}
}

func TestKiloSelectSessionRequiresExactIDAndTrueAcceptance(t *testing.T) {
	requests := 0
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		requests++
		if request.Method != http.MethodPost || request.URL.Path != "/tui/select-session" {
			http.NotFound(response, request)
			return
		}
		var body struct {
			SessionID string `json:"sessionID"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil {
			http.Error(response, "bad body", http.StatusBadRequest)
			return
		}
		switch body.SessionID {
		case "ses_exact":
			_, _ = response.Write([]byte("true"))
		case "ses_false":
			_, _ = response.Write([]byte("false"))
		case "ses_gone":
			http.Error(response, "gone", http.StatusNotFound)
		default:
			http.Error(response, "unexpected ID", http.StatusBadRequest)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectKilo, handler)
	defer closeClient()
	if err := client.SelectKiloSession(context.Background(), "ses_exact"); err != nil {
		t.Fatalf("exact select = %v", err)
	}
	if err := client.SelectKiloSession(context.Background(), "ses_false"); !errors.Is(err, productruntime.ErrNativeRejected) {
		t.Fatalf("false select = %v", err)
	}
	if err := client.SelectKiloSession(context.Background(), "ses_gone"); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("gone select = %v", err)
	}
	if err := client.SelectKiloSession(context.Background(), "forged"); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("invalid select = %v", err)
	}
	if requests != 3 {
		t.Fatalf("native select requests = %d", requests)
	}
}

func TestDeleteSessionConvergesWhenExactNativeIDIsAlreadyAbsent(t *testing.T) {
	requests := 0
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		requests++
		if request.Method != http.MethodDelete || request.URL.Path != "/session/ses_already_gone" {
			http.NotFound(response, request)
			return
		}
		http.Error(response, "already gone", http.StatusNotFound)
	})
	client, closeClient := newFamilyTestClient(t, DialectKilo, handler)
	defer closeClient()
	if err := client.DeleteSession(context.Background(), "ses_already_gone"); err != nil {
		t.Fatalf("delete already-absent exact native session = %v", err)
	}
	if requests != 1 {
		t.Fatalf("exact delete requests = %d", requests)
	}
}

func TestOpenCodeNoReplyAndAbortRequireExactNativeAcceptance(t *testing.T) {
	abortAccepted := false
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		switch request.Method + " " + request.URL.Path {
		case "POST /session/ses_exact/prompt_async":
			var body struct {
				MessageID string `json:"messageID"`
				NoReply   bool   `json:"noReply"`
				Parts     []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			if json.NewDecoder(request.Body).Decode(&body) != nil || body.MessageID == "" || !body.NoReply ||
				len(body.Parts) != 1 || body.Parts[0].Type != "text" || body.Parts[0].Text != "visible notice" {
				t.Errorf("no-reply body = %#v", body)
			}
			response.WriteHeader(http.StatusNoContent)
		case "POST /session/ses_exact/abort":
			_ = json.NewEncoder(response).Encode(abortAccepted)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	if _, err := client.PromptAsync(context.Background(), "ses_exact", "notice-one", []byte("visible notice"), true, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Interrupt(context.Background(), "ses_exact"); !errors.Is(err, productruntime.ErrNativeRejected) {
		t.Fatalf("false native abort acceptance = %v", err)
	}
	abortAccepted = true
	if err := client.Interrupt(context.Background(), "ses_exact"); err != nil {
		t.Fatalf("true native abort acceptance = %v", err)
	}
}

func TestKiloPermissionRelayAndInterruptUseV2Routes(t *testing.T) {
	var mu sync.Mutex
	replied, interrupted := false, false
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuthOnly(t, request)
		if request.URL.RawQuery != "" {
			t.Errorf("Kilo v2 request query = %q", request.URL.RawQuery)
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /api/session/ses_kilo/event":
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(response, "data: {\"type\":\"permission.v2.asked\",\"data\":{\"id\":\"per_kilo\",\"sessionID\":\"ses_kilo\",\"action\":\"bash\",\"resources\":[]}}\n\n")
			_, _ = fmt.Fprint(response, "data: {\"type\":\"session.turn.close\",\"properties\":{\"sessionID\":\"ses_kilo\",\"reason\":\"completed\"}}\n\n")
		case "POST /api/session/ses_kilo/permission/per_kilo/reply":
			var body map[string]string
			if json.NewDecoder(request.Body).Decode(&body) != nil || body["reply"] != "always" {
				t.Errorf("permission reply = %#v", body)
			}
			mu.Lock()
			replied = true
			mu.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case "POST /api/session/ses_kilo/interrupt":
			mu.Lock()
			interrupted = true
			mu.Unlock()
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	})
	client, closeClient := newFamilyTestClient(t, DialectKilo, handler)
	defer closeClient()
	if err := client.WaitIdle(context.Background(), "ses_kilo", func(context.Context, NativeEvent) (string, error) { return "always", nil }); err != nil {
		t.Fatal(err)
	}
	if err := client.Interrupt(context.Background(), "ses_kilo"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !replied || !interrupted {
		t.Fatalf("v2 permission replied=%v interrupted=%v", replied, interrupted)
	}
}

func TestDocumentProbeRequiresExactSupportedRoutesAndBoundsResponses(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuthOnly(t, request)
		if request.URL.Path == "/doc" {
			if request.URL.RawQuery != "" {
				t.Errorf("static document probe query = %q", request.URL.RawQuery)
			}
			_, _ = response.Write([]byte(`{"paths":{"/session":{},"/event":{}}}`))
			return
		}
		http.NotFound(response, request)
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	features, err := client.ProbeDocument(context.Background(), []string{"/session", "/event", "/experimental/unsafe"})
	if err != nil || !features["/session"] || !features["/event"] || features["/experimental/unsafe"] {
		t.Fatalf("features = %#v, %v", features, err)
	}
	if _, err := client.PromptAsync(context.Background(), "ses_exact", "receipt", []byte(strings.Repeat("x", maxResultBytes+1)), false, nil); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("oversized prompt = %v", err)
	}
}

func TestTurnReconciliationRejectsUnrelatedOrIncompleteAssistant(t *testing.T) {
	complete := false
	parentID := "msg_other"
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		if request.URL.Path != "/session/ses_exact/message" {
			http.NotFound(response, request)
			return
		}
		completed := ""
		if complete {
			completed = `,"time":{"completed":123}`
		}
		_, _ = fmt.Fprintf(response, `[{"info":{"id":"msg_input","sessionID":"ses_exact","role":"user"},"parts":[]},{"info":{"id":"msg_answer","sessionID":"ses_exact","role":"assistant","parentID":%q%s},"parts":[{"type":"text","text":"wrong turn"}]}]`, parentID, completed)
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()

	completed, err := client.TurnCompleted(context.Background(), "ses_exact", "msg_input")
	if err != nil || completed {
		t.Fatalf("unrelated assistant completed=%v err=%v", completed, err)
	}
	parentID = "msg_input"
	completed, err = client.TurnCompleted(context.Background(), "ses_exact", "msg_input")
	if err != nil || completed {
		t.Fatalf("incomplete assistant completed=%v err=%v", completed, err)
	}
	complete = true
	completed, err = client.TurnCompleted(context.Background(), "ses_exact", "msg_input")
	if err != nil || !completed {
		t.Fatalf("exact completed assistant completed=%v err=%v", completed, err)
	}
}
