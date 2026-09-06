package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sessionkit "github.com/antst/sessionbus/bus/sdk/go"
	"github.com/antst/sessionbus/wrappers/host"
)

type fakeRun struct {
	done, admitted chan struct{}
	once           sync.Once
	interrupted    bool
}

func newFakeRun() *fakeRun                       { return &fakeRun{done: make(chan struct{}), admitted: make(chan struct{})} }
func (r *fakeRun) Admitted()                     { r.once.Do(func() { close(r.admitted) }) }
func (r *fakeRun) AdmittedDone() <-chan struct{} { return r.admitted }
func (r *fakeRun) Done() <-chan struct{}         { return r.done }
func (r *fakeRun) Interrupted() bool             { return r.interrupted }

func testClient(server *httptest.Server) *nativeClient {
	return &nativeClient{endpoint: server.URL, username: "user", password: "pass", directory: "/work", http: server.Client()}
}

func TestHelloDescribesOnlyHonouredOpenSurface(t *testing.T) {
	hello, err := (&Wrapper{}).Hello(context.Background())
	if err != nil || hello.Product != Product || hello.Version != "1.18.29" || !slices.Equal(hello.SupportedOpenFields, []string{"cwd", "permission_mode", "model", "arguments"}) {
		t.Fatalf("hello = %#v/%v", hello, err)
	}
	if names := []string{hello.ExtraArguments[0].Name, hello.ExtraArguments[1].Name, hello.ExtraArguments[2].Name, hello.ExtraArguments[3].Name, hello.ExtraArguments[4].Name, hello.ExtraArguments[5].Name}; !slices.Equal(names, []string{"--agent", "--print-logs", "--log-level", "--mdns", "--mdns-domain", "--cors"}) {
		t.Fatalf("extra arguments = %#v", hello.ExtraArguments)
	}
}

func TestReadinessWaitsForExactPluginTool(t *testing.T) {
	tools := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if username, password, ok := request.BasicAuth(); !ok || username != "user" || password != "pass" {
			t.Fatalf("auth = %q/%q/%v", username, password, ok)
		}
		if request.URL.Path == "/doc" {
			paths := map[string]any{}
			for _, path := range []string{"/session", "/session/{sessionID}", "/event", "/api/session/{sessionID}/prompt", "/api/session/{sessionID}/wait", "/api/session/{sessionID}/message", "/api/session/{sessionID}/interrupt", "/api/session/{sessionID}/model", "/api/session/{sessionID}/agent", "/experimental/tool/ids"} {
				paths[path] = map[string]any{}
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"paths": paths})
			return
		}
		tools++
		if tools == 1 {
			_, _ = response.Write([]byte(`[]`))
		} else {
			_, _ = response.Write([]byte(`["sessionbus"]`))
		}
	}))
	defer server.Close()
	client := testClient(server)
	ready, err := client.ready(context.Background())
	if err != nil || ready {
		t.Fatalf("first ready = %v/%v", ready, err)
	}
	ready, err = client.ready(context.Background())
	if err != nil || !ready {
		t.Fatalf("second ready = %v/%v", ready, err)
	}
}

func TestReadinessRejectsNullToolInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/doc" {
			paths := map[string]any{}
			for _, path := range []string{"/session", "/session/{sessionID}", "/event", "/api/session/{sessionID}/prompt", "/api/session/{sessionID}/wait", "/api/session/{sessionID}/message", "/api/session/{sessionID}/interrupt", "/api/session/{sessionID}/model", "/api/session/{sessionID}/agent", "/experimental/tool/ids"} {
				paths[path] = map[string]any{}
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"paths": paths})
			return
		}
		_, _ = response.Write([]byte("null"))
	}))
	defer server.Close()
	if _, err := testClient(server).ready(context.Background()); err == nil || err.Error() != "OpenCode tool inventory is malformed" {
		t.Fatalf("error = %v", err)
	}
}

func TestNativeRepliesMustIdentifyCommittedState(t *testing.T) {
	rows := []struct {
		name, response, want string
		call                 func(*nativeClient) error
	}{
		{"create cwd", `{"id":"ses_exact","title":"title","directory":"/else"}`, "ambiguous session", func(client *nativeClient) error {
			_, err := client.create(context.Background(), "title", "ask")
			return err
		}},
		{"resume id", `{"id":"ses_other","title":"title","directory":"/work"}`, "different session", func(client *nativeClient) error { _, err := client.get(context.Background(), "ses_exact"); return err }},
		{"resume settings", `{"id":"ses_exact","title":"other","directory":"/work"}`, "confirm resumed session settings", func(client *nativeClient) error {
			_, err := client.update(context.Background(), "ses_exact", "title", "ask")
			return err
		}},
		{"prompt echo", `{"data":{"admittedSeq":0,"id":"msg_wrong","sessionID":"ses_exact","prompt":{"text":"go"},"delivery":"steer"}}`, "invalid input admission", func(client *nativeClient) error {
			_, err := client.prompt(context.Background(), "ses_exact", "msg_exact", "go", "steer", true)
			return err
		}},
		{"prompt session", `{"data":{"admittedSeq":0,"id":"msg_exact","sessionID":"ses_other","prompt":{"text":"go"},"delivery":"steer"}}`, "invalid input admission", func(client *nativeClient) error {
			_, err := client.prompt(context.Background(), "ses_exact", "msg_exact", "go", "steer", true)
			return err
		}},
		{"prompt text", `{"data":{"admittedSeq":0,"id":"msg_exact","sessionID":"ses_exact","prompt":{"text":"else"},"delivery":"steer"}}`, "invalid input admission", func(client *nativeClient) error {
			_, err := client.prompt(context.Background(), "ses_exact", "msg_exact", "go", "steer", true)
			return err
		}},
		{"prompt delivery", `{"data":{"admittedSeq":0,"id":"msg_exact","sessionID":"ses_exact","prompt":{"text":"go"},"delivery":"queue"}}`, "invalid input admission", func(client *nativeClient) error {
			_, err := client.prompt(context.Background(), "ses_exact", "msg_exact", "go", "steer", true)
			return err
		}},
		{"prompt negative sequence", `{"data":{"admittedSeq":-1,"id":"msg_exact","sessionID":"ses_exact","prompt":{"text":"go"},"delivery":"steer"}}`, "invalid input admission", func(client *nativeClient) error {
			_, err := client.prompt(context.Background(), "ses_exact", "msg_exact", "go", "steer", true)
			return err
		}},
		{"prompt sequence absent", `{"data":{"id":"msg_exact","sessionID":"ses_exact","prompt":{"text":"go"},"delivery":"steer"}}`, "invalid input admission", func(client *nativeClient) error {
			_, err := client.prompt(context.Background(), "ses_exact", "msg_exact", "go", "steer", true)
			return err
		}},
		{"delete false", `false`, "did not confirm session deletion", func(client *nativeClient) error { return client.remove(context.Background(), "ses_exact") }},
		{"permission false", `false`, "did not confirm permission rejection", func(client *nativeClient) error {
			return client.rejectPermission(context.Background(), "ses_exact", "per_exact")
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write([]byte(row.response)) }))
			defer server.Close()
			if err := row.call(testClient(server)); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNativeResponseBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(strings.Repeat("x", nativeLimit+1)))
	}))
	defer server.Close()
	if _, err := testClient(server).get(context.Background(), "ses_exact"); err == nil || err.Error() != "OpenCode response exceeds 1 MiB" {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateUsesExactPermissionRule(t *testing.T) {
	for _, action := range []string{"ask", "allow"} {
		t.Run(action, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				var body struct {
					Title      string              `json:"title"`
					Permission []map[string]string `json:"permission"`
				}
				if json.NewDecoder(request.Body).Decode(&body) != nil || body.Title != "title" || len(body.Permission) != 1 || body.Permission[0]["permission"] != "*" || body.Permission[0]["pattern"] != "*" || body.Permission[0]["action"] != action {
					t.Fatalf("body = %#v", body)
				}
				_ = json.NewEncoder(response).Encode(nativeSession{ID: "ses_exact", Title: "title", Directory: "/work"})
			}))
			defer server.Close()
			if _, err := testClient(server).create(context.Background(), "title", action); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResumeReadsThenAppliesAndVerifiesExactSettings(t *testing.T) {
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method)
		if request.URL.Path != "/session/ses_exact" || request.URL.Query().Get("directory") != "/work" {
			t.Fatalf("request = %s", request.URL)
		}
		if request.Method == http.MethodPatch {
			var body struct {
				Title      string              `json:"title"`
				Permission []map[string]string `json:"permission"`
			}
			if json.NewDecoder(request.Body).Decode(&body) != nil || body.Title != "Resumed title" || len(body.Permission) != 1 || body.Permission[0]["permission"] != "*" || body.Permission[0]["pattern"] != "*" || body.Permission[0]["action"] != "allow" {
				t.Fatalf("patch body = %#v", body)
			}
		}
		_ = json.NewEncoder(response).Encode(nativeSession{ID: "ses_exact", Title: "Resumed title", Directory: "/work"})
	}))
	defer server.Close()
	session, err := testClient(server).resume(context.Background(), "ses_exact", "Resumed title", "allow")
	if err != nil || session.ID != "ses_exact" || session.Title != "Resumed title" || !slices.Equal(methods, []string{http.MethodGet, http.MethodPatch}) {
		t.Fatalf("resume = %#v/%v, methods = %#v", session, err, methods)
	}
}

func TestConfigureUsesCapturedV2Shapes(t *testing.T) {
	requests := make(chan struct {
		path string
		body map[string]any
	}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if json.NewDecoder(request.Body).Decode(&body) != nil {
			t.Fatal("malformed request body")
		}
		requests <- struct {
			path string
			body map[string]any
		}{request.URL.Path, body}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := testClient(server).configure(context.Background(), "ses_exact", &modelRef{ProviderID: "openai", ModelID: "gpt"}, "build"); err != nil {
		t.Fatal(err)
	}
	model, agent := <-requests, <-requests
	if model.path != "/api/session/ses_exact/model" || fmt.Sprint(model.body) != "map[model:map[modelID:gpt providerID:openai]]" {
		t.Fatalf("model = %#v", model)
	}
	if agent.path != "/api/session/ses_exact/agent" || fmt.Sprint(agent.body) != "map[agent:build]" {
		t.Fatalf("agent = %#v", agent)
	}
}

func TestOpenBarrierWaitsForPluginAndReportsExit(t *testing.T) {
	previous := retryReady
	retryReady = func(context.Context) error { return nil }
	defer func() { retryReady = previous }()
	attempts := 0
	err := waitReady(context.Background(), make(chan struct{}), func(context.Context) (bool, error) {
		attempts++
		return attempts == 2, nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("wait = %v after %d attempts", err, attempts)
	}
	exited := make(chan struct{})
	close(exited)
	err = waitReady(context.Background(), exited, func(context.Context) (bool, error) { return false, nil })
	if err == nil || err.Error() != "OpenCode exited before plugin readiness" {
		t.Fatalf("exit error = %v", err)
	}
	retryReady = previous
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = waitReady(cancelled, make(chan struct{}), func(context.Context) (bool, error) { return false, nil }); err != context.Canceled {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestWrapperOpenOwnsServerPluginAndSessionLifecycle(t *testing.T) {
	directory, record := t.TempDir(), filepath.Join(t.TempDir(), "requests.jsonl")
	socket := filepath.Join(directory, "sessionbus.sock")
	t.Setenv("OPENCODE_TEST_CHILD", "1")
	t.Setenv("OPENCODE_TEST_RECORD", record)
	for _, name := range []string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		t.Setenv(name, "must-not-reach-child")
	}
	oldCommand := laneCommand
	laneCommand = func(_ string, arguments ...string) *exec.Cmd {
		return exec.Command(os.Args[0], append([]string{"-test.run=TestOpenCodeProcess", "--"}, arguments...)...)
	}
	t.Cleanup(func() { laneCommand = oldCommand })
	p := New(socket, "provisional")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	opened, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "Lane title@local", Open: sessionkit.OpenOptions{Cwd: directory, PermissionMode: "bypassPermissions", Model: "openai/gpt", Arguments: []string{"--agent", "build", "--log-level", "INFO", "--mdns"}}})
	if err != nil || opened.SessionID != "ses_exact" {
		t.Fatalf("open = %#v/%v", opened, err)
	}
	if err = p.Close(context.Background(), sessionkit.SessionCloseRequest{Forget: true}); err != nil {
		t.Fatal(err)
	}
	rows := readProcessRows(t, record)
	start := rows[0]
	if fmt.Sprint(start["args"]) != "[serve --hostname 127.0.0.1 --port "+fmt.Sprint(start["port"])+" --log-level INFO --mdns]" || start["lane_socket"] != filepath.Join(directory, "lanes", "provisional.sock") || start["auth_complete"] != true {
		t.Fatalf("start = %#v", start)
	}
	for _, name := range []string{host.SocketEnv, host.LocalKeyEnv, host.TokenEnv, host.SessionIDEnv, host.NameEnv, host.GroupsEnv} {
		if slices.ContainsFunc(start["env_names"].([]any), func(value any) bool { return value == name }) {
			t.Fatalf("%s reached child: %#v", name, start)
		}
	}
	joined := fmt.Sprint(rows[1:])
	for _, fragment := range []string{"GET /doc", "GET /experimental/tool/ids?directory=", "GET /event?directory=", "POST /session?directory=", `permission:[map[action:allow pattern:* permission:*]]`, `modelID:gpt`, `providerID:openai`, `agent:build`, "DELETE /session/ses_exact?directory="} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("request log omitted %q: %s", fragment, joined)
		}
	}
	if _, err = os.Stat(filepath.Join(directory, "locks", "opencode", "ses_exact")); err != nil {
		t.Fatalf("renamed lock: %v", err)
	}
	if _, err = os.Stat(filepath.Join(directory, "lanes", "provisional.sock")); !os.IsNotExist(err) {
		t.Fatalf("lane socket remains: %v", err)
	}
}

func TestOpenCodeProcess(t *testing.T) {
	if os.Getenv("OPENCODE_TEST_CHILD") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	arguments := os.Args[separator+1:]
	port := ""
	for index, argument := range arguments {
		if argument == "--port" && index+1 < len(arguments) {
			port = arguments[index+1]
		}
	}
	names := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		names = append(names, name)
	}
	record := os.Getenv("OPENCODE_TEST_RECORD")
	mode := os.Getenv("OPENCODE_TEST_MODE")
	var mu sync.Mutex
	promptID := ""
	write := func(value any) {
		mu.Lock()
		defer mu.Unlock()
		file, err := os.OpenFile(record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(file).Encode(value)
		_ = file.Close()
	}
	write(map[string]any{"args": arguments, "port": port, "env_names": names, "lane_socket": os.Getenv(LaneSocketEnv), "auth_complete": os.Getenv("OPENCODE_SERVER_USERNAME") == "sessionbus" && os.Getenv("OPENCODE_SERVER_PASSWORD") != ""})
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body any
		if request.Body != nil {
			_ = json.NewDecoder(request.Body).Decode(&body)
		}
		write(map[string]any{"request": request.Method + " " + request.URL.String(), "body": body})
		username, password, ok := request.BasicAuth()
		if !ok || username != "sessionbus" || password == "" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case request.URL.Path == "/doc":
			paths := map[string]any{}
			for _, path := range []string{"/session", "/session/{sessionID}", "/event", "/api/session/{sessionID}/prompt", "/api/session/{sessionID}/wait", "/api/session/{sessionID}/message", "/api/session/{sessionID}/interrupt", "/api/session/{sessionID}/model", "/api/session/{sessionID}/agent", "/experimental/tool/ids"} {
				paths[path] = map[string]any{}
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"paths": paths})
		case request.URL.Path == "/experimental/tool/ids":
			_ = json.NewEncoder(response).Encode([]string{ToolName})
		case request.URL.Path == "/event":
			response.Header().Set("Content-Type", "text/event-stream")
			response.WriteHeader(http.StatusOK)
			response.(http.Flusher).Flush()
			<-request.Context().Done()
		case request.Method == http.MethodPost && request.URL.Path == "/session":
			_ = json.NewEncoder(response).Encode(nativeSession{ID: "ses_exact", Title: "Lane title", Directory: request.URL.Query().Get("directory")})
		case request.Method == http.MethodPost && (strings.HasSuffix(request.URL.Path, "/model") || strings.HasSuffix(request.URL.Path, "/agent")):
			response.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/prompt"):
			input := body.(map[string]any)
			mu.Lock()
			promptID, _ = input["id"].(string)
			mu.Unlock()
			_ = json.NewEncoder(response).Encode(map[string]any{"data": map[string]any{"admittedSeq": 0, "id": input["id"], "sessionID": "ses_exact", "prompt": input["prompt"], "delivery": input["delivery"]}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/wait"):
			response.WriteHeader(http.StatusNoContent)
			response.(http.Flusher).Flush()
			if mode == "no-terminal-exit" {
				os.Exit(0)
			}
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/message") && mode == "terminal-exit":
			mu.Lock()
			messageID := promptID
			mu.Unlock()
			encoded, _ := json.Marshal(map[string]any{"data": []any{
				map[string]any{"id": messageID, "type": "user", "text": "large", "time": map[string]any{"created": 1}},
				map[string]any{"id": "msg_answer", "type": "assistant", "time": map[string]any{"created": 2, "completed": 3}, "content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 300004)}}, "finish": "stop"},
			}, "cursor": map[string]string{}})
			response.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
			_, _ = response.Write(encoded)
			response.(http.Flusher).Flush()
			os.Exit(0)
		case request.Method == http.MethodDelete && request.URL.Path == "/session/ses_exact":
			_ = json.NewEncoder(response).Encode(true)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, handler); err != nil {
		os.Exit(3)
	}
}

func readProcessRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Fatalf("process row = %q", line)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRunAdmissionPrecedesActiveDelivery(t *testing.T) {
	baseSeen, releaseBase := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	order := []string{}
	baseID := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/prompt"):
			var body struct {
				ID     string `json:"id"`
				Prompt struct {
					Text string `json:"text"`
				} `json:"prompt"`
				Delivery string `json:"delivery"`
				Resume   bool   `json:"resume"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			mu.Lock()
			order = append(order, body.Delivery)
			if len(order) == 1 {
				baseID = body.ID
			}
			position := len(order)
			mu.Unlock()
			if position == 1 {
				close(baseSeen)
				<-releaseBase
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"data": map[string]any{"admittedSeq": position - 1, "id": body.ID, "sessionID": "ses_exact", "prompt": map[string]string{"text": body.Prompt.Text}, "delivery": body.Delivery, "timeCreated": 1}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/wait"):
			response.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/message"):
			mu.Lock()
			id := baseID
			mu.Unlock()
			_ = json.NewEncoder(response).Encode(map[string]any{"data": []any{map[string]any{"id": id, "type": "user", "text": "base", "time": map[string]any{"created": 1}}, map[string]any{"id": "msg_answer", "type": "assistant", "time": map[string]any{"created": 2, "completed": 3}, "content": []any{map[string]any{"type": "text", "text": "done"}}, "finish": "stop"}}, "cursor": map[string]string{}})
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
	}))
	defer server.Close()
	p := &Wrapper{opened: true, id: "ses_exact", client: testClient(server)}
	run := newFakeRun()
	runResult := make(chan error, 1)
	go func() {
		result, err := p.run(context.Background(), run, "base")
		if err == nil && (result.Result != "done" || result.NativeStopReason != "stop") {
			err = fmt.Errorf("result = %#v", result)
		}
		runResult <- err
	}()
	<-baseSeen
	delivered := make(chan sessionkit.DeliveryReceipt, 1)
	deliveryErr := make(chan error, 1)
	go func() {
		receipt, err := p.deliver(context.Background(), delivery(), run)
		delivered <- receipt
		deliveryErr <- err
	}()
	select {
	case <-delivered:
		t.Fatal("delivery passed native run admission")
	default:
	}
	close(releaseBase)
	if err := <-runResult; err != nil {
		t.Fatal(err)
	}
	receipt := <-delivered
	if err := <-deliveryErr; err != nil || receipt.Disposition != "injected" {
		t.Fatalf("delivery = %#v/%v", receipt, err)
	}
	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()
	if !slices.Equal(got, []string{"steer", "steer"}) {
		t.Fatalf("native order = %#v", got)
	}
}

func TestFinishedRunQueuesDelivery(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewDecoder(request.Body).Decode(&requestBody)
		_ = json.NewEncoder(response).Encode(map[string]any{"data": map[string]any{"admittedSeq": 0, "id": requestBody["id"], "sessionID": "ses_exact", "prompt": requestBody["prompt"], "delivery": "queue", "timeCreated": 1}})
	}))
	defer server.Close()
	p := &Wrapper{opened: true, id: "ses_exact", client: testClient(server)}
	run := newFakeRun()
	run.Admitted()
	close(run.done)
	receipt, err := p.deliver(context.Background(), delivery(), run)
	if err != nil || receipt.Disposition != "queued_for_next_turn" || requestBody["resume"] != false {
		t.Fatalf("delivery = %#v/%v, body=%#v", receipt, err, requestBody)
	}
}

func TestInterruptWaitsForNativeAdmission(t *testing.T) {
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/session/ses_exact/interrupt" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		called <- struct{}{}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	p := &Wrapper{opened: true, id: "ses_exact", client: testClient(server)}
	run := newFakeRun()
	done := make(chan error, 1)
	go func() { done <- p.interrupt(context.Background(), run) }()
	select {
	case <-called:
		t.Fatal("interrupt preceded native run admission")
	default:
	}
	run.Admitted()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-called

	failed := newFakeRun()
	close(failed.done)
	if err := p.interrupt(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Fatal("failed run was interrupted natively")
	default:
	}
}

func TestIdleRunQueuesDelivery(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewDecoder(request.Body).Decode(&requestBody)
		_ = json.NewEncoder(response).Encode(map[string]any{"data": map[string]any{"admittedSeq": 0, "id": requestBody["id"], "sessionID": "ses_exact", "prompt": requestBody["prompt"], "delivery": "queue", "timeCreated": 1}})
	}))
	defer server.Close()
	p := &Wrapper{opened: true, id: "ses_exact", client: testClient(server)}
	receipt, err := p.deliver(context.Background(), delivery(), nil)
	if err != nil || receipt.Disposition != "queued_for_next_turn" || requestBody["resume"] != false {
		t.Fatalf("delivery = %#v/%v, body=%#v", receipt, err, requestBody)
	}
}

func TestCloseDeletesOnlyForExplicitForget(t *testing.T) {
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/session/ses_exact" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		deletes++
		_, _ = response.Write([]byte("true"))
	}))
	defer server.Close()
	for _, row := range []struct {
		forget bool
		want   int
	}{{false, 0}, {true, 1}} {
		p := &Wrapper{id: "ses_exact", client: testClient(server)}
		if err := p.Close(context.Background(), sessionkit.SessionCloseRequest{Forget: row.forget}); err != nil {
			t.Fatal(err)
		}
		if deletes != row.want {
			t.Fatalf("forget %v: deletes = %d", row.forget, deletes)
		}
	}
}

func TestPermissionEventIsRejected(t *testing.T) {
	rejected := make(chan map[string]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/event":
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = response.Write([]byte("data: {\"type\":\"permission.asked\",\"properties\":{\"id\":\"per_exact\",\"sessionID\":\"ses_exact\"}}\n\n"))
		case "/session/ses_exact/permissions/per_exact":
			var body map[string]string
			_ = json.NewDecoder(request.Body).Decode(&body)
			rejected <- body
			_, _ = response.Write([]byte("true"))
		default:
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
	}))
	defer server.Close()
	client := testClient(server)
	done, err := client.subscribe(context.Background(), func(ctx context.Context, event nativeEvent) error {
		return client.rejectPermission(ctx, event.Properties.SessionID, event.Properties.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := <-rejected; body["response"] != "reject" {
		t.Fatalf("permission body = %#v", body)
	}
	if err = <-done; err == nil || err.Error() != "OpenCode event stream ended" {
		t.Fatalf("stream end = %v", err)
	}
}

func TestResultProjectsEveryCompletedAssistantAfterInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"data":[{"id":"msg_before","type":"assistant","time":{"created":1,"completed":2},"content":[{"type":"text","text":"before"}],"finish":"stop"},{"id":"msg_input","type":"user","text":"go","time":{"created":3}},{"id":"msg_one","type":"assistant","time":{"created":4,"completed":5},"content":[{"type":"text","text":"one"}],"finish":"tool-calls"},{"id":"msg_two","type":"assistant","time":{"created":6,"completed":7},"content":[{"type":"text","text":"two"}],"finish":"stop"}],"cursor":{}}`))
	}))
	defer server.Close()
	result, stop, err := testClient(server).result(context.Background(), "ses_exact", "msg_input")
	if err != nil || result != "one\ntwo" || stop != "stop" {
		t.Fatalf("result = %q/%q/%v", result, stop, err)
	}
}

func TestResultRejectsIdleWithoutCompletedAssistant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"data":[{"id":"msg_input","type":"user","text":"go","time":{"created":1}}],"cursor":{}}`))
	}))
	defer server.Close()
	if _, _, err := testClient(server).result(context.Background(), "ses_exact", "msg_input"); err == nil || err.Error() != "OpenCode idle history omitted a completed assistant" {
		t.Fatalf("error = %v", err)
	}
}

func TestWrapperDrainsOwnedTerminalBeforeChildExitShutdown(t *testing.T) {
	testWrapperChildExit(t, "terminal-exit", true)
}

func TestWrapperFailsThenJoinsRunBeforeChildExitShutdown(t *testing.T) {
	testWrapperChildExit(t, "no-terminal-exit", false)
}

func testWrapperChildExit(t *testing.T, mode string, wantTerminal bool) {
	directory, record := t.TempDir(), filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("OPENCODE_TEST_CHILD", "1")
	t.Setenv("OPENCODE_TEST_MODE", mode)
	t.Setenv("OPENCODE_TEST_RECORD", record)
	oldCommand := laneCommand
	laneCommand = func(_ string, arguments ...string) *exec.Cmd {
		return exec.Command(os.Args[0], append([]string{"-test.run=TestOpenCodeProcess", "--"}, arguments...)...)
	}
	t.Cleanup(func() { laneCommand = oldCommand })
	p := New(filepath.Join(directory, "sessionbus.sock"), "provisional")
	p.SetCall(func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	shutdown := make(chan struct{})
	var shutdownOnce sync.Once
	p.SetShutdown(func() { shutdownOnce.Do(func() { close(shutdown) }) })
	if _, err := p.Open(context.Background(), sessionkit.OpenRequest{Name: "Lane title@local", Open: sessionkit.OpenOptions{Cwd: directory}}); err != nil {
		t.Fatal(err)
	}
	run := newFakeRun()
	result, err := p.run(context.Background(), run, "large")
	if wantTerminal {
		if err != nil || result.Outcome != "completed" || len(result.Result) != 300004 || result.NativeStopReason != "stop" {
			t.Fatalf("terminal = %#v/%v", result, err)
		}
	} else if err == nil {
		t.Fatalf("missing terminal completed: %#v", result)
	}
	select {
	case <-shutdown:
		t.Fatal("worker shutdown preceded Run.Done")
	default:
	}
	close(run.done)
	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("worker shutdown did not join Run.Done")
	}
	if err := p.Close(context.Background(), sessionkit.SessionCloseRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestResultKeepsAscendingOrderAcrossPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Query().Get("limit") != "100" {
			t.Fatalf("query = %s", request.URL.RawQuery)
		}
		if requests == 1 {
			if request.URL.Query().Get("order") != "asc" {
				t.Fatalf("initial order = %s", request.URL.RawQuery)
			}
			_, _ = response.Write([]byte(`{"data":[{"id":"msg_input","type":"user"}],"cursor":{"next":"page-2"}}`))
			return
		}
		if request.URL.Query().Get("cursor") != "page-2" || request.URL.Query().Has("order") {
			t.Fatalf("cursor = %s", request.URL.RawQuery)
		}
		_, _ = response.Write([]byte(`{"data":[{"id":"msg_answer","type":"assistant","time":{"completed":2},"content":[{"type":"text","text":"done"}],"finish":"stop"}],"cursor":{}}`))
	}))
	defer server.Close()
	result, stop, err := testClient(server).result(context.Background(), "ses_exact", "msg_input")
	if err != nil || result != "done" || stop != "stop" || requests != 2 {
		t.Fatalf("result = %q/%q/%v after %d requests", result, stop, err, requests)
	}
}

func TestOpenArgumentsAndInteractiveArity(t *testing.T) {
	arguments, model, agent, err := launchArguments(sessionkit.OpenOptions{PermissionMode: "default", Model: "openai/gpt", Arguments: []string{"--agent", "build", "--log-level", "INFO"}})
	if err != nil || !slices.Equal(arguments, []string{"--log-level", "INFO"}) || model.ProviderID != "openai" || model.ModelID != "gpt" || agent != "build" {
		t.Fatalf("arguments = %#v/%#v/%q/%v", arguments, model, agent, err)
	}
	if _, _, _, err = launchArguments(sessionkit.OpenOptions{Arguments: []string{"--pure"}}); err == nil || err.Error() != "unsupported argument --pure" {
		t.Fatalf("lane --pure = %v", err)
	}
	if _, _, _, err = launchArguments(sessionkit.OpenOptions{PermissionMode: "plan"}); err == nil || err.Error() != "unsupported value permission_mode=plan" {
		t.Fatalf("permission mode = %v", err)
	}
	arguments, _, agent, err = launchArguments(sessionkit.OpenOptions{Arguments: []string{"--log-level", "--agent"}})
	if err != nil || agent != "" || !slices.Equal(arguments, []string{"--log-level", "--agent"}) {
		t.Fatalf("native value arity = %#v/%q/%v", arguments, agent, err)
	}
	plan, native, err := InteractivePlan([]string{"--log-level", "-g", "team"}, []string{"PATH=/bin"})
	if err != nil || native || !slices.Equal(plan.Args, []string{"--log-level", "-g", "team"}) {
		t.Fatalf("arity plan = %#v/%v/%v", plan, native, err)
	}
	if !slices.Contains(plan.Env, host.SocketEnv+"="+sessionkit.Socket()) {
		t.Fatalf("default socket missing from %#v", plan.Env)
	}
	plan, native, err = InteractivePlan([]string{"run", "-g"}, []string{"PATH=/bin"})
	if err != nil || !native || !slices.Equal(plan.Args, []string{"run", "-g"}) {
		t.Fatalf("passthrough = %#v/%v/%v", plan, native, err)
	}
	plan, native, err = InteractivePlan([]string{"/work/project", "run", "-g", "team"}, []string{"PATH=/bin"})
	if err != nil || native || !slices.Equal(plan.Args, []string{"/work/project", "run"}) {
		t.Fatalf("project = %#v/%v/%v", plan, native, err)
	}
	if _, _, err = InteractivePlan([]string{"--pure=true"}, nil); err == nil {
		t.Fatal("--pure accepted")
	}
	plan, native, err = InteractivePlan([]string{"--log-level", "--pure"}, nil)
	if err != nil || native || !slices.Equal(plan.Args, []string{"--log-level", "--pure"}) {
		t.Fatalf("native pure value = %#v/%v/%v", plan, native, err)
	}
}

func TestNativeIDsFollowProductPrefixAndBusIdentityBounds(t *testing.T) {
	if !validNativeID("ses_日本") || validNativeID("ses bad") || validNativeID("ses_\xff") {
		t.Fatal("native session id boundary changed")
	}
	if !validPermissionID("per.dotted") || validPermissionID("request") {
		t.Fatal("native permission id prefix changed")
	}
}

func delivery() sessionkit.DeliveryRequest {
	return sessionkit.DeliveryRequest{MessageID: "delivery", From: sessionkit.DeliverySource{SessionID: "sender", Name: "Sender", Product: "codex", Groups: []string{}}, Body: "hello"}
}
