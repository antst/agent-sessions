package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestACPClientRejectsMalformedJSONRPCResponseShapes(t *testing.T) {
	tests := []struct {
		name  string
		frame func(any) map[string]any
	}{
		{"missing-result-and-error", func(id any) map[string]any { return map[string]any{"jsonrpc": "2.0", "id": id} }},
		{"both-result-and-error", func(id any) map[string]any {
			return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}, "error": map[string]any{"code": -32000, "message": "bad"}}
		}},
		{"method-mixed-with-result", func(id any) map[string]any {
			return map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission", "result": map[string]any{}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			process := newScriptedACPProcess()
			process.writeHook = func(request map[string]any) {
				body, err := json.Marshal(test.frame(request["id"]))
				if err != nil {
					t.Error(err)
					return
				}
				process.reads <- body
			}
			client, err := NewACPClient(process, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Initialize(context.Background()); !errors.Is(err, productruntime.ErrProtocol) {
				t.Fatalf("Initialize error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestACPClientRejectsNullRequiredResult(t *testing.T) {
	process := newScriptedACPProcess()
	process.writeHook = func(request map[string]any) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
		process.reads <- body
	}
	client, err := NewACPClient(process, nil)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Sessions []acpSessionInfo `json:"sessions"`
	}
	if err := client.Request(context.Background(), "session/list", map[string]string{"cwd": "/work"}, &listed); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("session/list null result error = %v, want ErrProtocol", err)
	}
}
