package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

const ToolName, ProtocolVersion = "agent_sessions", "2025-06-18"

type Backend interface {
	Call(context.Context, string, any) (json.RawMessage, error)
	Prepare(context.Context, json.RawMessage) error
}

type BackendFunc func(context.Context, string, any) (json.RawMessage, error)

func (call BackendFunc) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return call(ctx, method, params)
}
func (BackendFunc) Prepare(context.Context, json.RawMessage) error { return nil }

type Server struct {
	Backend  Backend
	mu       sync.Mutex
	writeErr error
}

type request struct {
	ID, Params json.RawMessage
	Method     string
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
	var caller *sessionkit.Caller
	source, owned := s.Backend.(interface{ Caller() *sessionkit.Caller })
	if owned {
		caller = source.Caller()
	} else {
		caller = sessionkit.NewCaller(s.Backend.Call)
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
			s.handle(ctx, caller, body, output)
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

func (s *Server) handle(ctx context.Context, caller *sessionkit.Caller, body []byte, output io.Writer) {
	request, failed := decodeRequest(body)
	if failed != nil {
		s.write(output, request.ID, nil, failed)
		return
	}
	if len(request.ID) == 0 {
		return
	}
	result, failed := s.call(ctx, caller, request)
	s.write(output, request.ID, result, failed)
}

func (s *Server) call(ctx context.Context, caller *sessionkit.Caller, request request) (any, *failure) {
	switch request.Method {
	case "initialize":
		return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "agent-sessions", "version": "unified"}}, nil
	case "tools/list":
		return map[string]any{"tools": []any{toolDefinition()}}, nil
	case "tools/call":
		return s.callTool(ctx, caller, request.Params)
	default:
		return nil, &failure{Code: -32601, Message: "Method not found: " + request.Method}
	}
}

func (s *Server) callTool(ctx context.Context, caller *sessionkit.Caller, raw json.RawMessage) (any, *failure) {
	var call struct {
		Name      string          `json:"name"`
		Meta      json.RawMessage `json:"_meta"`
		Arguments struct {
			Action    string          `json:"action"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"arguments"`
	}
	if json.Unmarshal(raw, &call) != nil || call.Name != ToolName || !slices.Contains(sessionkit.Actions, call.Arguments.Action) {
		return nil, &failure{Code: -32602, Message: "Invalid params"}
	}
	arguments := call.Arguments.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	} else if !object(arguments) {
		return nil, &failure{Code: -32602, Message: "Invalid params"}
	}
	if err := s.Backend.Prepare(ctx, call.Meta); err != nil {
		return nil, errorFailure(err)
	}
	result, err := caller.Action(ctx, call.Arguments.Action, arguments)
	if err != nil {
		return nil, errorFailure(err)
	}
	var structured any
	if !json.Valid(result) || json.Unmarshal(result, &structured) != nil {
		return nil, &failure{Code: -32603, Message: "Agentbus backend returned an invalid response"}
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(result)}}, "structuredContent": structured}, nil
}

func decodeRequest(body []byte) (request, *failure) {
	if !json.Valid(body) {
		return request{}, &failure{Code: -32700, Message: "Parse error"}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return request{}, &failure{Code: -32600, Message: "Invalid Request"}
	}
	id, head := fields["id"], bytes.TrimSpace(fields["id"])
	idOK := len(head) == 0 || head[0] == '"' || head[0] == '-' || head[0] >= '0' && head[0] <= '9'
	value, version := request{ID: id, Params: fields["params"]}, ""
	if !idOK {
		value.ID = nil
	}
	methodOK := json.Unmarshal(fields["method"], &value.Method) == nil && value.Method != ""
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" || !methodOK || !idOK {
		return value, &failure{Code: -32600, Message: "Invalid Request"}
	}
	return value, nil
}

func object(raw json.RawMessage) bool {
	body := bytes.TrimSpace(raw)
	return len(body) > 0 && body[0] == '{' && json.Valid(body)
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
				"action":    map[string]any{"type": "string", "enum": sessionkit.Actions, "description": "Exact Agent Sessions operation."},
				"arguments": map[string]any{"type": "object", "additionalProperties": true, "description": "Arguments in the exact shape for the selected operation."},
			},
		},
	}
}

func errorFailure(err error) *failure {
	var protocol *sessionkit.ProtocolError
	if errors.As(err, &protocol) {
		return &failure{Code: protocol.Code, Message: protocol.Message, Data: protocol.Data}
	}
	return &failure{Code: -32603, Message: err.Error()}
}
