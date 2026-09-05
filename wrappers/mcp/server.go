package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

const ToolName = "agent_sessions"

var Actions = []string{"list", "send", "spawn", "describe", "run", "start", "wait", "status", "interrupt", "close", "forget"}

type Backend interface {
	Call(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

type Server struct {
	Backend  Backend
	mu       sync.Mutex
	writeErr error
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type failure struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (s *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if s.Backend == nil || input == nil || output == nil {
		return errors.New("MCP server is incomplete")
	}
	if closer, ok := input.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var calls sync.WaitGroup
	for scanner.Scan() {
		body := append([]byte(nil), scanner.Bytes()...)
		if len(body) == 0 {
			continue
		}
		calls.Add(1)
		go func() {
			defer calls.Done()
			s.handle(ctx, body, output)
		}()
	}
	readErr := scanner.Err()
	done := make(chan struct{})
	go func() { calls.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if readErr != nil {
		return readErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}

func (s *Server) handle(ctx context.Context, body []byte, output io.Writer) {
	var request request
	if json.Unmarshal(body, &request) != nil {
		s.write(output, nil, nil, &failure{Code: -32700, Message: "Parse error"})
		return
	}
	if len(request.ID) == 0 {
		return
	}
	result, failed := s.call(ctx, request)
	s.write(output, request.ID, result, failed)
}

func (s *Server) call(ctx context.Context, request request) (any, *failure) {
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{"protocolVersion": params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "agent-sessions", "version": "unified"}}, nil
	case "tools/list":
		return map[string]any{"tools": []any{toolDefinition()}}, nil
	case "tools/call":
		return s.callTool(ctx, request.Params)
	default:
		return nil, &failure{Code: -32601, Message: "Method not found: " + request.Method}
	}
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *failure) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Action    string          `json:"action"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"arguments"`
	}
	if json.Unmarshal(raw, &call) != nil || call.Name != ToolName || !validAction(call.Arguments.Action) {
		return nil, &failure{Code: -32602, Message: "Invalid params"}
	}
	arguments := call.Arguments.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var object map[string]any
	if json.Unmarshal(arguments, &object) != nil {
		return nil, &failure{Code: -32602, Message: "Invalid params"}
	}
	result, err := s.Backend.Call(ctx, call.Arguments.Action, arguments)
	if err != nil {
		return nil, errorFailure(err)
	}
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	var structured any
	if !json.Valid(result) || json.Unmarshal(result, &structured) != nil {
		return nil, &failure{Code: -32603, Message: "Agentbus backend returned an invalid response"}
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(result)}}, "structuredContent": structured}, nil
}

func (s *Server) write(output io.Writer, id json.RawMessage, result any, failed *failure) {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if failed != nil {
		response["error"] = failed
	} else {
		response["result"] = result
	}
	s.mu.Lock()
	if err := json.NewEncoder(output).Encode(response); s.writeErr == nil {
		s.writeErr = err
	}
	s.mu.Unlock()
}

func toolDefinition() map[string]any {
	return map[string]any{
		"name":        ToolName,
		"description": "Use Agent Sessions to list or message peers and control product lanes.",
		"inputSchema": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"action"},
			"properties": map[string]any{
				"action":    map[string]any{"type": "string", "enum": Actions, "description": "Exact Agent Sessions operation."},
				"arguments": map[string]any{"type": "object", "additionalProperties": true, "description": "Arguments in the exact shape for the selected operation."},
			},
		},
	}
}

func validAction(action string) bool {
	for _, candidate := range Actions {
		if action == candidate {
			return true
		}
	}
	return false
}

func errorFailure(err error) *failure {
	var local *failure
	if errors.As(err, &local) {
		return local
	}
	var protocol *sessionkit.ProtocolError
	if errors.As(err, &protocol) {
		return &failure{Code: protocol.Code, Message: protocol.Message, Data: protocol.Data}
	}
	return &failure{Code: -32603, Message: err.Error()}
}

func (e *failure) Error() string { return e.Message }
