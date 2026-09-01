package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

const defaultMCPRelayFrameBytes = 2 << 20

var (
	// ErrConnectorInactive is returned when no exact managed attachment owns the caller.
	ErrConnectorInactive = errors.New("connector is inactive")
	// ErrMCPRelayFrameTooLarge identifies an input exceeding the configured vendor frame bound.
	ErrMCPRelayFrameTooLarge = errors.New("MCP relay frame exceeds configured bound")
)

// ConnectorAttestation is product evidence gathered from the native process
// boundary, never from model-supplied tool arguments.
type ConnectorAttestation struct {
	AttachmentID string
	Capability   string
	Evidence     daemonpkg.NativeEvidence
}

// MCPRelayConfig configures one stateless vendor-spawned stdio relay.
type MCPRelayConfig struct {
	Product       string
	Endpoint      string
	MaxFrameBytes int
	Generation    func(context.Context) (uint64, error)
	Attest        func(context.Context, json.RawMessage) (ConnectorAttestation, error)
	Refresh       func(context.Context) error
	RefreshEvery  time.Duration
}

// MCPRelay preserves vendor MCP stdio framing while forwarding each operation
// to the one daemon. It owns no registry, listener, or durable state.
type MCPRelay struct {
	config MCPRelayConfig
	client *daemonControlClient
}

type connectorRelayPayload struct {
	Product  string                   `json:"product"`
	Method   string                   `json:"method"`
	Params   json.RawMessage          `json:"params,omitempty"`
	Evidence daemonpkg.NativeEvidence `json:"evidence,omitempty"`
}

type mcpRelayRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpRelayFrame struct {
	line string
	err  error
}

type mcpRelayResponseFrame struct {
	id      json.RawMessage
	result  json.RawMessage
	failure *rpcError
}

// NewMCPRelay validates the fixed relay configuration.
func NewMCPRelay(config MCPRelayConfig) (*MCPRelay, error) {
	if _, ok := productcatalog.ByID(config.Product); !ok {
		return nil, fmt.Errorf("MCP relay product %q is unsupported", config.Product)
	}
	if config.Attest == nil {
		return nil, errors.New("MCP relay attestation callback is unavailable")
	}
	if config.MaxFrameBytes <= 0 {
		config.MaxFrameBytes = defaultMCPRelayFrameBytes
	}
	client, err := newDaemonControlClient(config.Endpoint, config.Generation)
	if err != nil {
		return nil, fmt.Errorf("configure MCP relay: %w", err)
	}
	return &MCPRelay{config: config, client: client}, nil
}

// Serve processes newline-delimited MCP JSON-RPC until EOF.
//
//nolint:gocyclo // JSON-RPC relay admission and response/error framing stay in one protocol loop.
func (r *MCPRelay) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if ctx == nil || input == nil || output == nil {
		return errors.New("MCP relay stream is incomplete")
	}
	scanner := bufio.NewScanner(input)
	initialBuffer := 4096
	if r.config.MaxFrameBytes < initialBuffer {
		initialBuffer = r.config.MaxFrameBytes
	}
	scanner.Buffer(make([]byte, initialBuffer), r.config.MaxFrameBytes)
	writer := bufio.NewWriter(output)
	defer func() { _ = writer.Flush() }()
	var refresh <-chan time.Time
	var refreshTicker *time.Ticker
	if r.config.Refresh != nil {
		interval := r.config.RefreshEvery
		if interval <= 0 {
			interval = time.Second
		}
		refreshTicker = time.NewTicker(interval)
		refresh = refreshTicker.C
		defer refreshTicker.Stop()
	}
	frames := make(chan mcpRelayFrame, 1)
	go func() {
		defer close(frames)
		for scanner.Scan() {
			select {
			case frames <- mcpRelayFrame{line: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case frames <- mcpRelayFrame{err: scanner.Err()}:
		case <-ctx.Done():
		}
	}()
	responses := make(chan mcpRelayResponseFrame)
	inFlight := 0
	for {
		if frames == nil && inFlight == 0 {
			return writer.Flush()
		}
		var frame mcpRelayFrame
		var ok bool
		refreshReady := refresh
		if inFlight > 0 {
			// Re-exec is safe only between protocol requests. A long-running lane
			// wait must not be replaced underneath its accepted response.
			refreshReady = nil
		}
		select {
		case <-ctx.Done():
			// A connector is a vendor-owned stdio child. Process shutdown must
			// not wait forever for the vendor to close stdin; returning lets the
			// multicall binary exit while leaving vendor infrastructure alone.
			return writer.Flush()
		case <-refreshReady:
			// Refresh only while no protocol frame is being handled. Successful
			// connector image replacement execs this process with the same stdio;
			// transient failures leave the current compatible relay available.
			_ = r.config.Refresh(ctx)
			continue
		case response := <-responses:
			if err := writeMCPRelayResponse(writer, response.id, response.result, response.failure); err != nil {
				return err
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flush MCP relay response: %w", err)
			}
			inFlight--
			continue
		case frame, ok = <-frames:
			if !ok {
				frames = nil
				continue
			}
		}
		if frame.err != nil {
			return fmt.Errorf("%w: %w", ErrMCPRelayFrameTooLarge, frame.err)
		}
		line := strings.TrimSpace(frame.line)
		if line == "" {
			continue
		}
		var request mcpRelayRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			if err := writeMCPRelayResponse(writer, nil, nil, &rpcError{Code: -32700, Message: "Parse error"}); err != nil {
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
		go func(request mcpRelayRequest) {
			result, rpcErr := r.handle(ctx, request)
			select {
			case responses <- mcpRelayResponseFrame{id: request.ID, result: result, failure: rpcErr}:
			case <-ctx.Done():
			}
		}(request)
	}
}

func (r *MCPRelay) handle(ctx context.Context, request mcpRelayRequest) (json.RawMessage, *rpcError) {
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
		return marshalMCPRelayResult(map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-sessions", "version": "unified"},
			"instructions":    ProductMCPInstructions(r.config.Product),
		}), nil
	case "tools/list":
		// MCP discovery must survive daemon restarts. The closed tool inventory is
		// part of the installed connector, while every stateful call is still
		// attested and forwarded to the sole daemon authority.
		return marshalMCPRelayResult(map[string]any{"tools": ProductMCPTools(r.config.Product)}), nil
	case "tools/call":
		attestation, err := r.config.Attest(ctx, append(json.RawMessage(nil), request.Params...))
		if err != nil || strings.TrimSpace(attestation.AttachmentID) == "" {
			// Native attestation failure is the expected bare-session boundary.
			//nolint:nilerr // Never expose product diagnostics or mutate daemon state for a bare caller.
			return marshalMCPRelayResult(inactiveMCPResult()), nil
		}
		return r.forward(ctx, request, "connector.call", attestation)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + request.Method}
	}
}

func (r *MCPRelay) forward(
	ctx context.Context,
	request mcpRelayRequest,
	operation string,
	attestation ConnectorAttestation,
) (json.RawMessage, *rpcError) {
	payload, err := json.Marshal(connectorRelayPayload{
		Product: r.config.Product, Method: request.Method,
		Params: append(json.RawMessage(nil), request.Params...), Evidence: cloneRelayEvidence(attestation.Evidence),
	})
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: "MCP relay request encoding failed"}
	}
	controlID := randomID()
	controlRequest := daemonpkg.ControlRequest{
		ID: controlID, Role: daemonpkg.RoleConnector, Operation: operation,
		AttachmentID: attestation.AttachmentID, Capability: attestation.Capability,
		Payload: json.RawMessage(payload),
	}
	if operation == "connector.call" {
		controlRequest.IdempotencyKey = controlID
	}
	response, err := r.client.call(ctx, controlRequest)
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: "Agent Sessions daemon is unavailable"}
	}
	if response.Error != nil {
		if response.Error.Code == daemonpkg.ErrorInactive {
			return marshalMCPRelayResult(inactiveMCPResult()), nil
		}
		if operation == "connector.call" {
			return marshalMCPRelayResult(map[string]any{
				"content": []map[string]any{{"type": "text", "text": response.Error.Message}}, "isError": true,
			}), nil
		}
		return nil, &rpcError{Code: -32603, Message: response.Error.Message}
	}
	if !response.OK || len(response.Payload) == 0 || !json.Valid(response.Payload) {
		return nil, &rpcError{Code: -32603, Message: "Agent Sessions daemon returned an invalid response"}
	}
	return append(json.RawMessage(nil), response.Payload...), nil
}

func writeMCPRelayResponse(writer io.Writer, id json.RawMessage, result json.RawMessage, failure *rpcError) error {
	response := map[string]any{"jsonrpc": "2.0"}
	if len(id) == 0 {
		response["id"] = nil
	} else {
		response["id"] = id
	}
	switch {
	case failure != nil:
		response["error"] = failure
	case len(result) == 0:
		response["result"] = map[string]any{}
	default:
		response["result"] = result
	}
	body, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode MCP relay response: %w", err)
	}
	body = append(body, '\n')
	if _, err := io.Copy(writer, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("write MCP relay response: %w", err)
	}
	return nil
}

func marshalMCPRelayResult(value any) json.RawMessage {
	body, _ := json.Marshal(value)
	return json.RawMessage(body)
}

func cloneRelayEvidence(evidence daemonpkg.NativeEvidence) daemonpkg.NativeEvidence {
	evidence.Ancestry = append([]procinfo.Identity(nil), evidence.Ancestry...)
	return evidence
}
