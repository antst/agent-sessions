package bridge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProjectionProbe is an opt-in diagnostic for a cloned CODEX_HOME. It is
// deliberately excluded from ordinary test runs so the bridge can compare
// public App Server history operations without ever touching a user's live
// thread store.
func TestProjectionProbe(t *testing.T) {
	if os.Getenv("CODEX_PROJECTION_PROBE") != "1" {
		t.Skip("set CODEX_PROJECTION_PROBE=1 against a disposable CODEX_HOME clone")
	}
	threadID := os.Getenv("CODEX_PROJECTION_THREAD_ID")
	if !validSessionID(threadID) {
		t.Fatal("CODEX_PROJECTION_THREAD_ID must be a valid thread UUID")
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		t.Fatal("CODEX_HOME must point at a disposable clone")
	}
	codexBin := firstEnv("CODEX_PROJECTION_CODEX_BIN", "codex")
	socketPath := filepath.Join(t.TempDir(), "app-server.sock")
	command := exec.Command(codexBin, "app-server", "--enable", "sqlite", "--listen", "unix://"+socketPath)
	command.Env = os.Environ()
	stderr := &strings.Builder{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start App Server: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		if output := strings.TrimSpace(stderr.String()); output != "" {
			t.Logf("App Server stderr:\n%s", output)
		}
	})

	var client *appServerClient
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		candidate, err := dialAppServer(ctx, socketPath)
		cancel()
		if err == nil {
			client = candidate
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("App Server did not become ready: %s", strings.TrimSpace(stderr.String()))
	}
	t.Cleanup(client.close)

	operations := strings.Split(firstEnv("CODEX_PROJECTION_OPERATIONS", "read-metadata"), ",")
	for _, operation := range operations {
		operation = strings.TrimSpace(operation)
		var output json.RawMessage
		var err error
		switch operation {
		case "read-metadata":
			err = requestWithTimeout(client, 60*time.Second, "thread/read", map[string]any{
				"threadId": threadID, "includeTurns": false,
			}, &output)
		case "read-full":
			err = requestWithTimeout(client, 60*time.Second, "thread/read", map[string]any{
				"threadId": threadID, "includeTurns": true,
			}, &output)
		case "turns-list":
			err = requestWithTimeout(client, 60*time.Second, "thread/turns/list", map[string]any{
				"threadId": threadID, "limit": 5, "sortDirection": "desc", "itemsView": "notLoaded",
			}, &output)
		case "resume-page":
			err = requestWithTimeout(client, 60*time.Second, "thread/resume", map[string]any{
				"threadId":     threadID,
				"excludeTurns": true,
				"initialTurnsPage": map[string]any{
					"limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
				},
			}, &output)
		case "resume-full":
			err = requestWithTimeout(client, 60*time.Second, "thread/resume", map[string]any{
				"threadId": threadID, "excludeTurns": false,
			}, &output)
		case "unsubscribe":
			err = requestWithTimeout(client, 60*time.Second, "thread/unsubscribe", map[string]any{
				"threadId": threadID,
			}, &output)
		default:
			t.Fatalf("unknown CODEX_PROJECTION_OPERATIONS entry %q", operation)
		}
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		t.Logf("%s response: %s", operation, summarizeProbeResponse(output))
	}
}

func summarizeProbeResponse(value json.RawMessage) string {
	var response struct {
		Thread struct {
			ID    string    `json:"id"`
			Turns []appTurn `json:"turns"`
		} `json:"thread"`
		InitialTurnsPage struct {
			Data []appTurn `json:"data"`
		} `json:"initialTurnsPage"`
		Data []appTurn `json:"data"`
	}
	if json.Unmarshal(value, &response) != nil {
		return string(value)
	}
	summary := map[string]any{"responseBytes": len(value)}
	if response.Thread.ID != "" {
		summary["threadId"] = response.Thread.ID
		summary["threadTurns"] = summarizeProbeTurns(response.Thread.Turns)
	}
	if response.InitialTurnsPage.Data != nil {
		summary["initialTurnsPage"] = summarizeProbeTurns(response.InitialTurnsPage.Data)
	}
	if response.Data != nil {
		summary["data"] = summarizeProbeTurns(response.Data)
	}
	encoded, _ := json.Marshal(summary)
	return string(encoded)
}

func summarizeProbeTurns(turns []appTurn) map[string]any {
	ids := make([]string, 0, len(turns))
	statuses := make([]string, 0, len(turns))
	for _, turn := range turns {
		ids = append(ids, turn.ID)
		statuses = append(statuses, normalizeStatus(turn.Status))
	}
	return map[string]any{"count": len(turns), "ids": ids, "statuses": statuses}
}
