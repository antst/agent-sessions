package sessiontools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

const defaultMCPRelayFrameBytes = 2 << 20

var ErrMCPRelayFrameTooLarge = errors.New("MCP relay frame exceeds configured bound")

type ConnectorAttestation struct {
	AttachmentID string
	Capability   string
	Evidence     daemon.NativeEvidence
}

type MCPRelayConfig struct {
	Product       string
	Endpoint      string
	MaxFrameBytes int
	Generation    func(context.Context) (uint64, error)
	Attest        func(context.Context, json.RawMessage) (ConnectorAttestation, error)
	Refresh       func(context.Context) error
	RefreshEvery  time.Duration
}

type MCPRelay struct {
	config MCPRelayConfig
	client *daemonControlClient
}

// ConnectorRelayPayload is the private daemon control payload. It is exported
// only so compatibility tests/wrappers can decode the frozen schema.
type ConnectorRelayPayload struct {
	Product  string                `json:"product"`
	Method   string                `json:"method"`
	Params   json.RawMessage       `json:"params,omitempty"`
	Evidence daemon.NativeEvidence `json:"evidence,omitempty"`
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
}

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
	var refresh <-chan time.Time
	var ticker *time.Ticker
	if r.config.Refresh != nil {
		interval := r.config.RefreshEvery
		if interval <= 0 {
			interval = time.Second
		}
		ticker = time.NewTicker(interval)
		refresh = ticker.C
		defer ticker.Stop()
	}
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
		refreshReady := refresh
		if inFlight > 0 {
			refreshReady = nil
		}
		select {
		case <-ctx.Done():
			return writer.Flush()
		case <-refreshReady:
			_ = r.config.Refresh(ctx)
		case response := <-responses:
			if err := writeRelayResponse(writer, response.id, response.result, response.failure); err != nil {
				return err
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flush MCP relay response: %w", err)
			}
			inFlight--
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
		attestation, err := r.config.Attest(ctx, append(json.RawMessage(nil), request.Params...))
		if err != nil || strings.TrimSpace(attestation.AttachmentID) == "" {
			return marshalRelayResult(inactiveResult()), nil
		}
		return r.forward(ctx, request, attestation)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + request.Method}
	}
}

func (r *MCPRelay) forward(ctx context.Context, request relayRequest, attestation ConnectorAttestation) (json.RawMessage, *rpcError) {
	payload, err := json.Marshal(ConnectorRelayPayload{Product: r.config.Product, Method: request.Method, Params: request.Params, Evidence: cloneEvidence(attestation.Evidence)})
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: "MCP relay request encoding failed"}
	}
	id := stableMCPRelayOperationID(r.config.Product, request)
	response, err := r.client.call(ctx, daemon.ControlRequest{ID: id, Role: daemon.RoleConnector, Operation: "connector.call", IdempotencyKey: id, AttachmentID: attestation.AttachmentID, Capability: attestation.Capability, Payload: payload})
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: "Agent Sessions daemon is unavailable"}
	}
	if response.Error != nil {
		if response.Error.Code == daemon.ErrorInactive {
			return marshalRelayResult(inactiveResult()), nil
		}
		return marshalRelayResult(map[string]any{"content": []map[string]any{{"type": "text", "text": response.Error.Message}}, "isError": true}), nil
	}
	if !response.OK || len(response.Payload) == 0 || !json.Valid(response.Payload) {
		return nil, &rpcError{Code: -32603, Message: "Agent Sessions daemon returned an invalid response"}
	}
	return append(json.RawMessage(nil), response.Payload...), nil
}

func inactiveResult() map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": daemon.CanonicalInactiveMessage}}, "isError": true}
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
func cloneEvidence(evidence daemon.NativeEvidence) daemon.NativeEvidence {
	evidence.Ancestry = append([]procinfo.Identity(nil), evidence.Ancestry...)
	return evidence
}
func stableMCPRelayOperationID(product string, request relayRequest) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("agent-sessions-mcp-operation\x00"))
	_, _ = digest.Write([]byte(product))
	_, _ = digest.Write([]byte{'\x00'})
	_, _ = digest.Write([]byte(request.Method))
	_, _ = digest.Write([]byte{'\x00'})
	_, _ = digest.Write(request.ID)
	_, _ = digest.Write([]byte{'\x00'})
	_, _ = digest.Write(request.Params)
	return "mcp-" + hex.EncodeToString(digest.Sum(nil)[:16])
}

type daemonControlClient struct {
	endpoint   string
	generation func(context.Context) (uint64, error)
	mu         sync.Mutex
	current    uint64
}

func newDaemonControlClient(endpoint string, generation func(context.Context) (uint64, error)) (*daemonControlClient, error) {
	if strings.TrimSpace(endpoint) == "" || generation == nil {
		return nil, errors.New("daemon control endpoint or generation source is unavailable")
	}
	return &daemonControlClient{endpoint: endpoint, generation: generation}, nil
}

func (c *daemonControlClient) call(ctx context.Context, request daemon.ControlRequest) (daemon.ControlResponse, error) {
	generation, err := c.readGeneration(ctx, false)
	if err != nil {
		return daemon.ControlResponse{}, err
	}
	request.Generation = generation
	response, err := daemon.CallControl(ctx, c.endpoint, request)
	if err != nil || response.Error == nil || response.Error.Code != daemon.ErrorStaleGeneration {
		return response, err
	}
	generation, err = c.readGeneration(ctx, true)
	if err != nil {
		return daemon.ControlResponse{}, err
	}
	request.Generation = generation
	return daemon.CallControl(ctx, c.endpoint, request)
}

func (c *daemonControlClient) readGeneration(ctx context.Context, refresh bool) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != 0 && !refresh {
		return c.current, nil
	}
	generation, err := c.generation(ctx)
	if err != nil || generation == 0 {
		return 0, errors.New("daemon generation is unavailable")
	}
	c.current = generation
	return generation, nil
}
