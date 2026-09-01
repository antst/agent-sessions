package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const grokFakeProcessEnv = "AGENT_SESSIONS_GROK_FAKE_PROCESS"

func TestGrokFakeProcess(_ *testing.T) {
	if os.Getenv(grokFakeProcessEnv) != "1" {
		return
	}
	runGrokFakeACP()
}

func runGrokFakeACP() {
	scanner := bufio.NewScanner(os.Stdin)
	var activeTurnUntil time.Time
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		recordGrokFake(map[string]any{"kind": "request", "request": request, "pid": os.Getpid()})
		method := stringValue(request["method"])
		result := map[string]any{}
		switch method {
		case "initialize":
			result["protocolVersion"] = "1"
			if os.Getenv("GROK_FAKE_NO_CACHED_TOKEN") != "1" {
				result["authMethods"] = []map[string]any{{"id": "cached_token", "name": "Cached token"}}
			}
		case "authenticate":
			if os.Getenv("GROK_FAKE_AUTH_REJECT") == "1" {
				writeGrokFakeResponse(request["id"], nil, map[string]any{"code": -32001, "message": "bad cached token"})
				continue
			}
		case "session/new", "session/load":
			if os.Getenv("GROK_FAKE_REQUIRE_AGENT_SESSIONS_MCP") == "1" {
				params, _ := request["params"].(map[string]any)
				servers, _ := params["mcpServers"].([]any)
				server := map[string]any{}
				if len(servers) == 1 {
					server, _ = servers[0].(map[string]any)
				}
				args, _ := server["args"].([]any)
				if len(servers) != 1 || stringValue(server["name"]) != "agent_sessions" ||
					!filepath.IsAbs(stringValue(server["command"])) || len(args) != 1 || stringValue(args[0]) != "grok-mcp" ||
					!reflect.DeepEqual(server["env"], []any{}) {
					writeGrokFakeResponse(request["id"], nil, map[string]any{"code": -32602, "message": "missing injected agent_sessions MCP"})
					continue
				}
			}
			result["sessionId"] = defaultString(os.Getenv("GROK_FAKE_GENERATED_SESSION_ID"), os.Getenv(grokSessionIDEnv))
		case "session/prompt":
			if delay, _ := strconv.Atoi(os.Getenv("GROK_FAKE_PROMPT_DELAY_MS")); delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			params, _ := request["params"].(map[string]any)
			writeGrokFakeNotification("session/update", map[string]any{
				"sessionId": stringValue(params["sessionId"]),
				"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{
					"type": "text", "text": defaultString(os.Getenv("GROK_FAKE_ANSWER"), "fake Grok lane answer"),
				}},
			})
			result["stopReason"] = "end_turn"
		case "session/cancel":
			continue
		case "_x.ai/sessions/list":
			if delay := time.Until(activeTurnUntil); delay > 0 {
				time.Sleep(delay)
			}
			if os.Getenv("GROK_FAKE_BAD_ROSTER") == "1" {
				result["result"] = map[string]any{"sessions": []any{}}
				break
			}
			yolo := os.Getenv("GROK_FAKE_YOLO") == "1"
			if path := os.Getenv("GROK_FAKE_YOLO_FILE"); path != "" {
				body, _ := os.ReadFile(path)
				yolo = strings.TrimSpace(string(body)) == "1"
			}
			activity := defaultString(strings.TrimSpace(os.Getenv("GROK_FAKE_ACTIVITY")), "idle")
			result["result"] = map[string]any{"sessions": []any{map[string]any{
				"sessionId": defaultString(os.Getenv("GROK_FAKE_GENERATED_SESSION_ID"), os.Getenv(grokSessionIDEnv)),
				"title":     strings.TrimSpace(os.Getenv("GROK_FAKE_SESSION_TITLE")), "resident": true,
				"yolo": yolo, "activity": activity,
			}}}
		case "_x.ai/mcp/call":
			params, _ := request["params"].(map[string]any)
			arguments, _ := params["arguments"].(map[string]any)
			result["result"] = map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "peer inventory ready"}},
				"structuredContent": map[string]any{"sessionId": stringValue(arguments["session_id"]), "status": "starting"},
				"isError":           false,
			}
		case "_x.ai/interject":
			result["result"] = map[string]any{"status": "queued"}
			inner, _ := request["params"].(map[string]any)
			notification := map[string]any{
				"sessionId": stringValue(inner["sessionId"]), "interjectionId": stringValue(inner["interjectionId"]),
				"text": stringValue(inner["text"]),
			}
			writeGrokFakeNotification("_x.ai/session/interjection", notification)
		}
		writeGrokFakeResponse(request["id"], result, nil)
	}
}

func writeGrokFakeNotification(method string, params map[string]any) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	_, _ = fmt.Fprintln(os.Stdout, string(body))
}

func writeGrokFakeResponse(id any, result, rpcErr map[string]any) {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	body, _ := json.Marshal(response)
	_, _ = fmt.Fprintln(os.Stdout, string(body))
}

func recordGrokFake(value map[string]any) {
	path := os.Getenv("GROK_FAKE_RECORD")
	if path == "" {
		return
	}
	body, _ := json.Marshal(value)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(body, '\n'))
	_ = file.Close()
}
