package sessiontools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"golang.org/x/sys/unix"
)

const defaultMCPRelayFrameBytes = 2 << 20

var ErrMCPRelayFrameTooLarge = errors.New("MCP relay frame exceeds configured bound")

type MCPRelayConfig struct {
	Product         string
	MaxFrameBytes   int
	Call            func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	Refresh         func(context.Context) error
	RefreshIdentity func() string
	afterRead       func()
}
type MCPRelay struct{ config MCPRelayConfig }
type relayRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type relayFrame struct {
	line     string
	err      error
	buffered int
}
type relayResponse struct {
	id, result json.RawMessage
	failure    *rpcError
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
	if config.RefreshIdentity == nil {
		config.RefreshIdentity = func() string { return "" }
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
	reader := bufio.NewReaderSize(input, min(4096, r.config.MaxFrameBytes+1))
	writer := bufio.NewWriter(output)
	defer func() { _ = writer.Flush() }()
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	var waitReady func() error
	readDone := func() error { return nil }
	if r.config.Refresh != nil {
		file, ok := input.(*os.File)
		if !ok {
			return errors.New("refresh-enabled MCP relay input must be an OS file")
		}
		cancelRead, cancelWrite, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("open MCP relay cancellation pipe: %w", err)
		}
		context.AfterFunc(readCtx, func() { _ = cancelWrite.Close() })
		readDone = cancelRead.Close
		waitReady = func() error { return pollRelayInput(readCtx, file, cancelRead) }
	}
	frames := make(chan relayFrame, 1)
	ready := make(chan struct{})
	permit := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	go r.readFrames(readCtx, reader, waitReady, readDone, ready, permit, frames, readerDone)
	if waitReady != nil {
		defer func() { stopRead(); <-readerDone }()
	}
	responses := make(chan relayResponse)
	readerReady, readerBusy, inFlight, buffered := false, false, 0, 0
	permitRead := func() {
		if readerReady && (r.config.RefreshIdentity() == "" || buffered > 0) {
			permit <- struct{}{}
			readerReady, readerBusy = false, true
		}
	}
	refresh := func() error {
		if r.config.Refresh == nil || r.config.RefreshIdentity() == "" || readerBusy || inFlight != 0 || buffered != 0 {
			return nil
		}
		if err := r.config.Refresh(ctx); err != nil {
			return err
		}
		if r.config.RefreshIdentity() != "" {
			return errors.New("installed connector image does not match the ready daemon")
		}
		permitRead()
		return nil
	}
	for {
		if err := refresh(); err != nil {
			return err
		}
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
		case <-ready:
			readerReady = true
			permitRead()
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				readerBusy = false
				continue
			}
			readerBusy, buffered = false, frame.buffered
			if frame.err != nil {
				return fmt.Errorf("%w: %w", ErrMCPRelayFrameTooLarge, frame.err)
			}
			identity := r.config.RefreshIdentity()
			if strings.TrimSpace(frame.line) == "" {
				continue
			}
			var request relayRequest
			if err := json.Unmarshal([]byte(frame.line), &request); err != nil {
				if err := writeRelayResponse(writer, nil, nil, &rpcError{Code: -32700, Message: "Parse error"}); err != nil {
					return err
				}
				if err := writer.Flush(); err != nil {
					return fmt.Errorf("flush MCP relay response: %w", err)
				}
				continue
			}
			if len(request.ID) == 0 {
				continue
			}
			request.ID = append(json.RawMessage(nil), request.ID...)
			request.Params = append(json.RawMessage(nil), request.Params...)
			inFlight++
			go func(request relayRequest, staleIdentity string) {
				var result json.RawMessage
				var rpcErr *rpcError
				if staleIdentity != "" {
					rpcErr = &rpcError{Code: -32003, Message: "Agent Sessions connector image is stale; retry after automatic refresh", Data: map[string]any{"reason": "stale_connector", "release_identity": staleIdentity}}
				} else {
					result, rpcErr = r.handle(ctx, request)
				}
				select {
				case responses <- relayResponse{id: request.ID, result: result, failure: rpcErr}:
				case <-ctx.Done():
				}
			}(request, identity)
		}
	}
}

func (r *MCPRelay) readFrames(ctx context.Context, reader *bufio.Reader, waitReady, done func() error, ready chan<- struct{}, permit <-chan struct{}, frames chan<- relayFrame, finished chan<- struct{}) {
	defer func() { _ = done(); close(finished); close(frames) }()
	for {
		if reader.Buffered() == 0 && waitReady != nil {
			if err := waitReady(); err != nil {
				if ctx.Err() != nil {
					return
				}
				frames <- relayFrame{err: err}
				return
			}
		}
		select {
		case ready <- struct{}{}:
		case <-ctx.Done():
			return
		}
		select {
		case <-permit:
		case <-ctx.Done():
			return
		}
		var line []byte
		var err error
		for more := true; more; {
			var part []byte
			part, more, err = reader.ReadLine()
			line = append(line, part...)
			if len(line) > r.config.MaxFrameBytes {
				err, more = ErrMCPRelayFrameTooLarge, false
			}
		}
		if r.config.afterRead != nil {
			r.config.afterRead()
		}
		if errors.Is(err, io.EOF) {
			return
		}
		select {
		case frames <- relayFrame{line: string(line), err: err, buffered: reader.Buffered()}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// Connector hosts are Unix-only in 0.4.0; polling leaves bytes unread until permitted.
func pollRelayInput(ctx context.Context, input, cancel *os.File) error {
	fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN | unix.POLLHUP}, {Fd: int32(cancel.Fd()), Events: unix.POLLIN | unix.POLLHUP}}
	for {
		_, err := unix.Poll(fds, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if fds[1].Revents != 0 {
			return ctx.Err()
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP) == 0 {
			return fmt.Errorf("poll MCP relay input returned events %#x", fds[0].Revents)
		}
		return nil
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
	if invalid := ValidateMCPToolCall(r.config.Product, request.Params); invalid != nil {
		return nil, &rpcError{Code: -32602, Message: "Invalid params", Data: invalid}
	}
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
