package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

var forwardMCPToDaemon = daemonpkg.ForwardMCP

// RunDaemonMCPRelay runs the one stateless stdio relay contract for an exact
// authoritative product. Product-specific payloads differ only in this
// attested connector identity; tools and routing remain daemon-owned.
func RunDaemonMCPRelay(product string, input io.Reader, output io.Writer, diagnostics io.Writer) int {
	if _, ok := bridgeProductByID(product); !ok {
		_, _ = fmt.Fprintf(diagnostics, "agent-sessions MCP relay: unsupported product %q\n", product)
		return 2
	}
	return runDaemonMCPRelay(product, input, output, diagnostics)
}

func runDaemonMCPRelay(product string, input io.Reader, output io.Writer, diagnostics io.Writer) int {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 2*maxFrameBytes)
	writer := bufio.NewWriter(output)
	defer func() { _ = writer.Flush() }()
	identity := daemonpkg.InheritedConnectorIdentity(product)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal([]byte(line), &request) != nil || request.Method == "" {
			writeMCPResponse(writer, nil, nil, &rpcError{Code: -32700, Message: "Parse error"})
			_ = writer.Flush()
			continue
		}
		if len(request.ID) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		decision, err := forwardMCPToDaemon(ctx, identity, request.Method, request.Params)
		cancel()
		switch {
		case err != nil:
			if request.Method == "tools/call" {
				writeMCPResponse(writer, request.ID, inactiveMCPResult(), nil)
			} else {
				writeMCPResponse(writer, request.ID, nil, &rpcError{Code: -32603, Message: "agent_sessions daemon unavailable"})
			}
			_, _ = fmt.Fprintf(diagnostics, "agent-sessions MCP relay: daemon request failed: %v\n", boundedRelayError(err))
		case decision.Error != nil:
			writeMCPResponse(writer, request.ID, nil, &rpcError{Code: decision.Error.Code, Message: decision.Error.Message})
		default:
			var result any
			if err := json.Unmarshal(decision.Result, &result); err != nil {
				writeMCPResponse(writer, request.ID, nil, &rpcError{Code: -32603, Message: "daemon returned an invalid MCP result"})
			} else {
				writeMCPResponse(writer, request.ID, result, nil)
			}
		}
		_ = writer.Flush()
	}
	if err := scanner.Err(); err != nil {
		_, _ = fmt.Fprintf(diagnostics, "agent-sessions MCP relay framing failed: %v\n", boundedRelayError(err))
		return 1
	}
	return 0
}

func boundedRelayError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func daemonMCPToolCall(ctx context.Context, product, tool string, arguments map[string]any) (daemonpkg.MCPForwardResult, error) {
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": arguments})
	if err != nil {
		return daemonpkg.MCPForwardResult{}, err
	}
	return forwardMCPToDaemon(ctx, daemonpkg.InheritedConnectorIdentity(product), "tools/call", params)
}

func decodeDaemonMCPToolResult(decision daemonpkg.MCPForwardResult) (mcpToolCallResponse, error) {
	if decision.Error != nil {
		return mcpToolCallResponse{}, fmt.Errorf("daemon MCP error %d: %s", decision.Error.Code, decision.Error.Message)
	}
	if len(decision.Result) == 0 {
		return mcpToolCallResponse{}, errors.New("daemon returned an empty MCP tool result")
	}
	var result mcpToolCallResponse
	if err := json.Unmarshal(decision.Result, &result); err != nil {
		return mcpToolCallResponse{}, err
	}
	return result, nil
}
