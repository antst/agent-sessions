package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

func runConnector(ctx context.Context, product string, output io.Writer) error {
	requestedProduct := product
	resolved, err := resolveConnectorProduct(product, os.Getenv)
	if err != nil {
		return err
	}
	product = resolved
	if connectorDeclinesForeignManagedProduct(requestedProduct, product, os.Getenv) {
		return nil
	}
	stateRoot := defaultStateRoot()
	refresher, err := newConnectorImageRefresher(os.Args, os.Environ())
	if err != nil {
		return fmt.Errorf("track installed connector image: %w", err)
	}
	// Normalize source-tree placeholders and stale pre-upgrade argv before the
	// vendor observes a ready MCP server. A successful refresh replaces this
	// process image and never returns; the replacement sees the exact current
	// identity and continues without another exec.
	if err := refresher.refresh(); err != nil {
		return fmt.Errorf("normalize installed connector image: %w", err)
	}
	report, reported := connectorLiveReport(product, os.Getenv)
	if reported && product == connectorProductClaude && report.Name == "" {
		report.Name = claudeActiveSessionName(ctx, report.UUID)
	}
	var live *liveSessionClient
	if reported && connectorClaimsLivePresence(product, os.Getenv) {
		live = startLiveSessionClient(ctx, livePresenceEndpoint(stateRoot), report,
			func(callCtx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
				return connectorNativeCall(callCtx, report, method, params)
			})
	}
	relay, err := sessiontools.NewMCPRelay(sessiontools.MCPRelayConfig{
		Product: product,
		Refresh: func(context.Context) error {
			return refresher.refresh()
		},
		Call: func(callCtx context.Context, id, method string, params json.RawMessage) (json.RawMessage, error) {
			if method == "tools/call" {
				cwd, cwdErr := os.Getwd()
				if cwdErr != nil {
					return nil, fmt.Errorf("read connector invocation cwd: %w", cwdErr)
				}
				params, cwdErr = stampConnectorInvocationCwd(params, cwd)
				if cwdErr != nil {
					return nil, cwdErr
				}
			}
			if live == nil {
				sourceID := connectorRelaySource(product, report.UUID, os.Getenv(launcher.QwenEventsFileEnv))
				return callConnectorDaemonTool(callCtx, product, sourceID, id, method, params)
			}
			return live.Call(callCtx, id, method, params)
		},
	})
	if err != nil {
		return err
	}
	return relay.Serve(ctx, os.Stdin, output)
}

func connectorRelaySource(product, ambient, qwenEventsPath string) string {
	if ambient != "" || product != connectorProductQwen {
		return ambient
	}
	sourceID, _ := qwenLaunchSessionID(qwenEventsPath)
	return sourceID
}

func connectorDeclinesForeignManagedProduct(requestedProduct, resolvedProduct string, getenv func(string) string) bool {
	if requestedProduct == "auto" {
		return false
	}
	managedProduct := strings.TrimSpace(getenv("AGENT_SESSIONS_PRODUCT"))
	return managedProduct != "" && managedProduct != resolvedProduct
}

func callConnectorDaemonTool(
	ctx context.Context, product string,
	sourceID, requestID, method string,
	params json.RawMessage,
) (json.RawMessage, error) {
	if method != "tools/call" {
		return nil, fmt.Errorf("connector method %s is unsupported", method)
	}
	var call connectorToolCall
	if json.Unmarshal(params, &call) != nil || strings.TrimSpace(call.Name) == "" {
		return nil, fmt.Errorf("connector tool call is invalid")
	}
	sourceID = connectorToolSource(product, sourceID, params, call.Arguments)
	payload, err := json.Marshal(connectorToolEnvelope{
		SourceID: sourceID, RequestID: requestID, Name: call.Name, Arguments: call.Arguments,
		Cwd: call.Metadata.InvocationCwd,
	})
	if err != nil {
		return nil, err
	}
	id := commandRequestID()
	response, err := callExistingDaemon(ctx, defaultStateRoot(), daemonpkg.ControlRequest{
		ID: id, Role: daemonpkg.RoleLauncher, Operation: "connector.tool",
		Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, errors.New(response.Error.Message)
	}
	return append(json.RawMessage(nil), response.Payload...), nil
}

func stampConnectorInvocationCwd(params json.RawMessage, cwd string) (json.RawMessage, error) {
	var call map[string]any
	if json.Unmarshal(params, &call) != nil {
		return nil, errors.New("connector tool call is invalid")
	}
	metadata, _ := call["_meta"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		call["_meta"] = metadata
	}
	metadata["agent-sessions/cwd"] = cwd
	return json.Marshal(call)
}

func connectorToolSource(product, ambient string, params json.RawMessage, arguments map[string]any) string {
	if product == connectorProductCodex {
		sourceID, _ := sessiontools.StdioMCPThreadID(params)
		delete(arguments, "session_id")
		return sourceID
	}
	sourceID := strings.TrimSpace(ambient)
	if sourceID == "" {
		sourceID = strings.TrimSpace(mapString(arguments, "session_id"))
	}
	delete(arguments, "session_id")
	return sourceID
}

func connectorNativeCall(ctx context.Context, report liveSessionReport, method string, params json.RawMessage) (json.RawMessage, error) {
	if method != "message.deliver" {
		return nil, fmt.Errorf("live session method %s is unsupported", method)
	}
	messageID, body, err := liveMessageRequest(params)
	if err != nil {
		return nil, err
	}
	adapter := connectorNativeAdapters[report.Product]
	if adapter == nil {
		return nil, fmt.Errorf("product %s has no Go connector delivery adapter", report.Product)
	}
	if err := adapter(ctx, report, messageID, body); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

func liveMessageRequest(params json.RawMessage) (string, string, error) {
	var request struct {
		MessageID string `json:"message_id"`
		Body      string `json:"body"`
	}
	if json.Unmarshal(params, &request) != nil || strings.TrimSpace(request.MessageID) == "" {
		return "", "", fmt.Errorf("live message delivery is invalid")
	}
	return request.MessageID, request.Body, nil
}

type connectorNativeAdapter func(context.Context, liveSessionReport, string, string) error

const (
	connectorProductCodex  = "codex"
	connectorProductClaude = "claude"
	connectorProductQwen   = "qwen"
	connectorProductGrok   = "grok"
)

var connectorNativeAdapters = map[string]connectorNativeAdapter{
	connectorProductCodex:  deliverCodexConnectorMessage,
	connectorProductClaude: deliverClaudeConnectorMessage,
}

// Products with a native presence adapter already own the session's one live
// stream. A project-discovered MCP connector may expose the same tool name,
// but it must never open a second connection and replace the product report.
func connectorOwnsLivePresence(product string) bool {
	return product == connectorProductClaude
}

func connectorClaimsLivePresence(product string, getenv func(string) string) bool {
	managedProduct := strings.TrimSpace(getenv("AGENT_SESSIONS_PRODUCT"))
	if managedProduct != "" && managedProduct != product {
		return false
	}
	return connectorOwnsLivePresence(product)
}

func deliverCodexConnectorMessage(ctx context.Context, report liveSessionReport, _ string, body string) error {
	native, err := bridge.OpenCodexNative(ctx, bridge.CodexNativeConfig{
		CodexBinary: codexBinary(), CodexHome: codexHome(), Environment: os.Environ(),
	})
	if err != nil {
		return err
	}
	defer native.Close()
	_, err = native.SendMessage(ctx, report.UUID, body)
	return err
}

func deliverClaudeConnectorMessage(_ context.Context, _ liveSessionReport, messageID, body string) error {
	return sendClaudeConnectorMessage(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), messageID, body)
}

func qwenNativeSessionInfo(home, sessionID string) (string, string, bool) {
	return bridge.QwenNativeSessionInfo(home, sessionID)
}

func submitQwenNativeInput(path, body string) error {
	writer, err := bridge.OpenQwenNativeInput(path)
	if err != nil {
		return err
	}
	defer writer.Close()
	return writer.Submit(body)
}

func sendClaudeConnectorMessage(socket, messageID, message string) error {
	if strings.TrimSpace(socket) == "" {
		return fmt.Errorf("Claude messaging socket is unavailable")
	}
	body, err := json.Marshal(map[string]any{
		"msgV": 1, "msg_id": messageID, "type": "user", "priority": "next",
		"from": "agent-sessions", "message": map[string]any{"role": "user", "content": message},
	})
	if err != nil {
		return err
	}
	connection, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = connection.Write(append(body, '\n'))
	return err
}

func connectorLiveReport(product string, getenv func(string) string) (liveSessionReport, bool) {
	uuid := ""
	if product == connectorProductGrok {
		uuid = strings.TrimSpace(getenv("GROK_SESSION_ID"))
	}
	productSessionEnv := map[string]string{
		laneCandidateCodex: "CODEX_THREAD_ID", laneCandidateClaude: "CLAUDE_CODE_SESSION_ID", laneCandidateGrok: "AGENT_SESSIONS_GROK_SESSION_ID",
	}
	if name := productSessionEnv[product]; uuid == "" && name != "" {
		uuid = strings.TrimSpace(getenv(name))
	}
	if uuid == "" {
		uuid = strings.TrimSpace(getenv("AGENT_SESSIONS_SESSION_ID"))
	}
	if uuid == "" {
		for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "AGENT_SESSIONS_GROK_SESSION_ID"} {
			if uuid = strings.TrimSpace(getenv(name)); uuid != "" {
				break
			}
		}
	}
	if uuid == "" {
		return liveSessionReport{}, false
	}
	name := strings.TrimSpace(getenv("AGENT_SESSIONS_SESSION_NAME"))
	var groups []string
	if encoded := strings.TrimSpace(getenv("AGENT_SESSIONS_GROUPS")); encoded != "" {
		if json.Unmarshal([]byte(encoded), &groups) != nil {
			return liveSessionReport{}, false
		}
	}
	return liveSessionReport{UUID: uuid, Name: name, Groups: uniqueStrings(groups), Product: product}, true
}

func claudeActiveSessionName(ctx context.Context, sessionID string) string {
	executable, err := launcher.ResolveProductExecutable(connectorProductClaude)
	if err != nil {
		return ""
	}
	payload, err := exec.CommandContext(ctx, executable, "agents", "--json").Output() //nolint:gosec // selected native Claude executable.
	if err != nil {
		return ""
	}
	return claudeActiveSessionNameFromJSON(payload, sessionID)
}

func claudeActiveSessionNameFromJSON(payload []byte, sessionID string) string {
	var sessions []struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
	}
	if json.Unmarshal(payload, &sessions) != nil {
		return ""
	}
	name := ""
	for _, session := range sessions {
		candidate := strings.TrimSpace(session.Name)
		if session.SessionID != sessionID || session.Kind != "interactive" || candidate == "" {
			continue
		}
		if name != "" && name != candidate {
			return ""
		}
		name = candidate
	}
	return name
}

// resolveConnectorProduct keeps the repository-root MCP manifest useful for
// products whose Go connector owns their live adapter. Products with a native
// extension still report themselves on their own stream, so their discovered
// MCP subprocess stays a harmless Codex-family relay with no live connection.
func resolveConnectorProduct(requested string, getenv func(string) string) (string, error) {
	if requested != "auto" {
		if _, ok := productcatalog.ByID(requested); !ok {
			return "", fmt.Errorf("connector product %q is unsupported", requested)
		}
		return requested, nil
	}
	product := strings.TrimSpace(getenv("AGENT_SESSIONS_PRODUCT"))
	if product != "" {
		if _, ok := productcatalog.ByID(product); !ok {
			return "", fmt.Errorf("automatic connector product %q is unsupported", product)
		}
		return connectorProductCodex, nil
	}
	if strings.TrimSpace(getenv("AGENT_SESSIONS_GROK_SESSION_ID")) != "" {
		product = "grok"
	}
	if product == "" {
		product = "codex"
	}
	if _, ok := productcatalog.ByID(product); !ok {
		return "", fmt.Errorf("automatic connector product %q is unsupported", product)
	}
	return product, nil
}
