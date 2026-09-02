package codebuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestReplyLivePeerUsesProductOwnedWorkerAndExactSession(t *testing.T) {
	const sessionID = "native-session"
	replied := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Header.Get(CSRFHeader) != CSRFValue {
			http.Error(response, "missing product header", http.StatusForbidden)
			return
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/sessions/live":
			_ = json.NewEncoder(response).Encode(map[string]any{"data": map[string]any{"sessionId": sessionID}})
		case "POST /api/v1/sessions/native-session/reply":
			var input struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(request.Body).Decode(&input)
			replied = input.Text
			_ = json.NewEncoder(response).Encode(map[string]any{"data": map[string]any{"delivered": true}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	directory := filepath.Join(home, ".codebuddy", "sessions")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(map[string]any{"sessionId": sessionID, "kind": "interactive", "endpoint": server.URL})
	if err := os.WriteFile(filepath.Join(directory, "worker.json"), row, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplyLivePeer(context.Background(), sessionID, "hello"); err != nil {
		t.Fatal(err)
	}
	if replied != "hello" {
		t.Fatalf("native reply = %q", replied)
	}
}
