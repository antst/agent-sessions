package kilocode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

type kiloPeerFixture struct {
	t          *testing.T
	sessionID  string
	marker     string
	directory  string
	server     *httptest.Server
	raw        *productserver.Client
	client     *TUIClient
	mu         sync.Mutex
	messages   []map[string]any
	background []BackgroundProcess
	messageSeq int
}

func newKiloPeerFixture(t *testing.T, sessionID, marker, directory string) *kiloPeerFixture {
	t.Helper()
	fixture := &kiloPeerFixture{t: t, sessionID: sessionID, marker: marker, directory: directory}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	auth, err := productserver.NewBasicAuth("agent-sessions", productruntime.NewSensitiveValue("pair-secret"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.raw, err = productserver.NewClient(productserver.ClientConfig{Endpoint: fixture.server.URL, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	fixture.client, err = NewTUIClient(fixture.raw, directory, func() time.Time { return time.Unix(50, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *kiloPeerFixture) close() {
	fixture.raw.CloseIdleConnections()
	fixture.server.Close()
}

func (fixture *kiloPeerFixture) handle(response http.ResponseWriter, request *http.Request) {
	username, password, ok := request.BasicAuth()
	if !ok || username != "agent-sessions" || password != "pair-secret" || request.URL.Query().Get("directory") != fixture.directory {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	switch request.Method + " " + request.URL.Path {
	case "GET /session/" + fixture.sessionID + "/message":
		_ = json.NewEncoder(response).Encode(fixture.messages)
	case "POST /tui/clear-prompt":
		_, _ = response.Write([]byte("true"))
	case "POST /tui/append-prompt":
		var body struct {
			Text string `json:"text"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil {
			http.Error(response, "bad prompt", http.StatusBadRequest)
			return
		}
		fixture.marker = body.Text
		_, _ = response.Write([]byte("true"))
	case "POST /tui/submit-prompt":
		fixture.messageSeq++
		fixture.messages = append(fixture.messages, map[string]any{
			"info":  map[string]any{"id": fmt.Sprintf("msg_%s_%d", fixture.sessionID, fixture.messageSeq), "sessionID": fixture.sessionID, "role": "user"},
			"parts": []map[string]string{{"type": "text", "text": fixture.marker}},
		})
		_, _ = response.Write([]byte("true"))
	case "GET /background-process":
		_ = json.NewEncoder(response).Encode(fixture.background)
	case "POST /mcp":
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(response).Encode(map[string]any{body.Name: map[string]string{"status": "connected"}})
	default:
		http.NotFound(response, request)
	}
}

func TestTwoIsolatedKiloFullAttachPairsNeverCrossDeliver(t *testing.T) {
	pairA := newKiloPeerFixture(t, "ses_pair_a", "", "/work/a")
	defer pairA.close()
	pairB := newKiloPeerFixture(t, "ses_pair_b", "", "/work/b")
	defer pairB.close()

	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan productruntime.NativeAcceptance, 2)
	errorsCh := make(chan error, 2)
	go func() {
		defer wait.Done()
		accepted, err := pairA.client.Deliver(context.Background(), "ses_pair_a", []byte("ROUTE_A_EXACT"))
		results <- accepted
		errorsCh <- err
	}()
	go func() {
		defer wait.Done()
		accepted, err := pairB.client.Deliver(context.Background(), "ses_pair_b", []byte("ROUTE_B_EXACT"))
		results <- accepted
		errorsCh <- err
	}()
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for accepted := range results {
		seen[accepted.NativeSessionID] = true
		if accepted.NativeMessageID == "" || !accepted.AcceptedAt.Equal(time.Unix(50, 0)) {
			t.Fatalf("acceptance = %#v", accepted)
		}
	}
	if !seen["ses_pair_a"] || !seen["ses_pair_b"] || pairA.server.URL == pairB.server.URL {
		t.Fatalf("isolated results = %#v, endpoints %s %s", seen, pairA.server.URL, pairB.server.URL)
	}
	pairA.mu.Lock()
	aJSON, _ := json.Marshal(pairA.messages)
	pairA.mu.Unlock()
	pairB.mu.Lock()
	bJSON, _ := json.Marshal(pairB.messages)
	pairB.mu.Unlock()
	if stringContains(string(aJSON), "ROUTE_B_EXACT") || stringContains(string(bJSON), "ROUTE_A_EXACT") {
		t.Fatalf("cross delivery A=%s B=%s", aJSON, bJSON)
	}
}

func TestKiloRejectsEveryAttachSelectionTopologyAuthAndReplayOverride(t *testing.T) {
	for _, arguments := range [][]string{
		{"--mini"}, {"--mini=true"}, {"--session", "ses_forged"}, {"--continue"}, {"-c"}, {"--fork"}, {"--cloud-fork"},
		{"--dir=/foreign"}, {"serve"}, {"daemon"}, {"--port", "1234"}, {"--password", "forged"}, {"-p=forged"},
		{"--username", "forged"}, {"-u=forged"}, {"--replay"}, {"--replay=false"}, {"--no-replay"}, {"--replay-limit", "20"},
	} {
		if !rejectMiniOrTopology(arguments) {
			t.Fatalf("managed Kilo accepted attach override %v", arguments)
		}
	}
	if rejectMiniOrTopology([]string{"--model", "kilo/free"}) {
		t.Fatal("ordinary full-attach option rejected")
	}
}

func TestKiloBackgroundAttributionAndMCPAreExactToPair(t *testing.T) {
	pair := newKiloPeerFixture(t, "ses_parent", "", "/work/parent")
	defer pair.close()
	pair.background = []BackgroundProcess{{ID: "bgp_exact", SessionID: "ses_parent", PID: 444, Cwd: "/work/parent", Status: "ready"}}
	process, err := pair.client.AttributeBackgroundProcess(context.Background(), 444, "ses_parent")
	if err != nil || process.ID != "bgp_exact" {
		t.Fatalf("background process = %#v, %v", process, err)
	}
	if _, err := pair.client.AttributeBackgroundProcess(context.Background(), 444, "ses_forged"); err == nil {
		t.Fatal("forged background session accepted")
	}
	if err := pair.client.RegisterMCP(context.Background(), "agent-sessions", []string{"agent-sessions", "connector"}); err != nil {
		t.Fatal(err)
	}
	if err := pair.client.RegisterMCP(context.Background(), "Agent Sessions", []string{"connector"}); err == nil {
		t.Fatal("invalid MCP registration name accepted")
	}
}

func stringContains(value, wanted string) bool {
	for index := 0; index+len(wanted) <= len(value); index++ {
		if value[index:index+len(wanted)] == wanted {
			return true
		}
	}
	return false
}
