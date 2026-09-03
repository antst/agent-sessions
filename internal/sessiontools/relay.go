package sessiontools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const defaultMCPRelayFrameBytes = 2 << 20

var ErrMCPRelayFrameTooLarge = errors.New("MCP relay frame exceeds configured bound")

type MCPRelayConfig struct {
	Product       string
	MaxFrameBytes int
	Call          func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	Refresh       func(context.Context) error
}

type MCPRelay struct {
	config MCPRelayConfig
}

type relayRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type relayFrame struct {
	line string
	err  error
}
type relayResponse struct {
	id      json.RawMessage
	result  json.RawMessage
	failure *rpcError
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type structuredRPCError interface {
	error
	RPCErrorDetails() (int, string, json.RawMessage)
}

func NewMCPRelay(config MCPRelayConfig) (*MCPRelay, error) {
	if _, ok := productcatalog.ByID(config.Product); !ok {
		return nil, fmt.Errorf("MCP relay product %q is unsupported", config.Product)
	}
	if config.Call == nil {
		return nil, errors.New("MCP relay live-session call is unavailable")
	}
	if config.MaxFrameBytes <= 0 {
		config.MaxFrameBytes = defaultMCPRelayFrameBytes
	}
	return &MCPRelay{config: config}, nil
}

// Serve preserves newline-delimited MCP JSON-RPC while forwarding only
// attested stateful operations to the daemon.
//
//nolint:gocyclo
func (r *MCPRelay) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if ctx == nil || input == nil || output == nil {
		return errors.New("MCP relay stream is incomplete")
	}
	scanner := bufio.NewScanner(input)
	initial := 4096
	if r.config.MaxFrameBytes < initial {
		initial = r.config.MaxFrameBytes
	}
	scanner.Buffer(make([]byte, initial), r.config.MaxFrameBytes)
	writer := bufio.NewWriter(output)
	defer func() { _ = writer.Flush() }()
	frames := make(chan relayFrame, 1)
	go func() {
		defer close(frames)
		for scanner.Scan() {
			select {
			case frames <- relayFrame{line: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case frames <- relayFrame{err: scanner.Err()}:
		case <-ctx.Done():
		}
	}()
	responses := make(chan relayResponse)
	inFlight := 0
	for {
		if frames == nil && inFlight == 0 {
			return writer.Flush()
		}
		select {
		case <-ctx.Done():
			return writer.Flush()
		case response := <-responses:
			if err := writeRelayResponse(writer, response.id, response.result, response.failure); err != nil {
				return err
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flush MCP relay response: %w", err)
			}
			inFlight--
			if r.config.Refresh != nil && inFlight == 0 {
				_ = r.config.Refresh(ctx)
			}
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			if frame.err != nil {
				return fmt.Errorf("%w: %w", ErrMCPRelayFrameTooLarge, frame.err)
			}
			if strings.TrimSpace(frame.line) == "" {
				continue
			}
			var request relayRequest
			if err := json.Unmarshal([]byte(frame.line), &request); err != nil {
				if err := writeRelayResponse(writer, nil, nil, &rpcError{Code: -32700, Message: "Parse error"}); err != nil {
					return err
				}
				continue
			}
			if len(request.ID) == 0 {
				continue
			}
			request.ID = append(json.RawMessage(nil), request.ID...)
			request.Params = append(json.RawMessage(nil), request.Params...)
			inFlight++
			go func(request relayRequest) {
				result, rpcErr := r.handle(ctx, request)
				select {
				case responses <- relayResponse{id: request.ID, result: result, failure: rpcErr}:
				case <-ctx.Done():
				}
			}(request)
		}
	}
}

func (r *MCPRelay) handle(ctx context.Context, request relayRequest) (json.RawMessage, *rpcError) {
	switch request.Method {
	case "ping":
		return json.RawMessage(`{}`), nil
	case "initialize":
		var input map[string]any
		_ = json.Unmarshal(request.Params, &input)
		protocol, _ := input["protocolVersion"].(string)
		if protocol == "" {
			protocol = "2025-06-18"
		}
		instruction, err := ProductMCPInstructions(r.config.Product)
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: err.Error()}
		}
		return marshalRelayResult(map[string]any{"protocolVersion": protocol, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "agent-sessions", "version": "unified"}, "instructions": instruction}), nil
	case "tools/list":
		tools, err := ProductMCPTools(r.config.Product)
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: err.Error()}
		}
		return marshalRelayResult(map[string]any{"tools": tools}), nil
	case "tools/call":
		return r.forward(ctx, request)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + request.Method}
	}
}

func (r *MCPRelay) forward(ctx context.Context, request relayRequest) (json.RawMessage, *rpcError) {
	id := string(request.ID)
	response, err := r.config.Call(ctx, id, request.Method, append(json.RawMessage(nil), request.Params...))
	if err != nil {
		var structured structuredRPCError
		if errors.As(err, &structured) {
			code, message, data := structured.RPCErrorDetails()
			return nil, &rpcError{Code: code, Message: message, Data: data}
		}
		return nil, &rpcError{Code: -32006, Message: err.Error(), Data: map[string]any{
			"detail": err.Error(), "agent_sessions_bug_report": BugReportGuidance,
		}}
	}
	if len(response) == 0 || !json.Valid(response) {
		return nil, &rpcError{Code: -32603, Message: "Agent Sessions daemon returned an invalid response"}
	}
	return append(json.RawMessage(nil), response...), nil
}

func writeRelayResponse(writer io.Writer, id, result json.RawMessage, failure *rpcError) error {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if len(id) == 0 {
		response["id"] = nil
	}
	if failure != nil {
		response["error"] = failure
	} else if len(result) == 0 {
		response["result"] = map[string]any{}
	} else {
		response["result"] = result
	}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	_, err = io.Copy(writer, bytes.NewReader(body))
	return err
}

func marshalRelayResult(value any) json.RawMessage { body, _ := json.Marshal(value); return body }
