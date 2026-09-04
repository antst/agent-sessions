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
	"github.com/antst/agent-sessions/internal/livepresence"
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
	refresher, err := newConnectorImageRefresher(os.Args, os.Environ())
	if err != nil {
		return fmt.Errorf("track installed connector image: %w", err)
	}
	report, reported := connectorLiveReport(product, os.Getenv)
	var resolveReport func(context.Context) (livepresence.Report, bool)
	if product == connectorProductClaude {
		report, reported = connectorReportFields(product, os.Getenv)
		report.UUID, report.Name = "", ""
		parentPID := os.Getppid()
		base := livepresence.CloneReport(report)
		resolveReport = func(resolveCtx context.Context) (livepresence.Report, bool) {
			return claudeActiveSessionForParent(resolveCtx, parentPID, base)
		}
	}
	if reported {
		cwd, cwdErr := os.Getwd()
		if cwdErr == nil {
			report.Info = livepresence.CwdInfo(cwd)
		}
	}
	if reported && product == connectorProductGrok && report.Name != "" {
		observer, observerErr := openGrokConnectorObserver(ctx, report)
		if observerErr != nil {
			return fmt.Errorf("open Grok connector for native rename: %w", observerErr)
		}
		renameErr := observer.Rename(ctx, report.Name)
		observer.Close()
		if renameErr != nil {
			return fmt.Errorf("rename Grok session: %w", renameErr)
		}
	}
	if reported && product == connectorProductGrok && report.Name == "" {
		report.Name = report.UUID
	}
	var live *livepresence.Client
	if reported && connectorClaimsLivePresence(requestedProduct, product, os.Getenv) {
		options := livepresence.ClientOptions{BeforePublish: connectorBeforePublish(refresher, connectorDaemonReleaseIdentity, os.Stderr)}
		if product == connectorProductClaude {
			base := livepresence.CloneReport(report)
			live = livepresence.StartClientWithOptions(ctx, defaultPresenceEndpoint(), livepresence.Report{}, resolveReport, func(callCtx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
				return connectorNativeCall(callCtx, base, method, params)
			}, options)
		} else {
			live = livepresence.StartClientWithOptions(ctx, defaultPresenceEndpoint(), report, nil,
				func(callCtx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
					return connectorNativeCall(callCtx, report, method, params)
				}, options)
		}
	}
	relay, err := sessiontools.NewMCPRelay(sessiontools.MCPRelayConfig{
		Product: product, RefreshIdentity: refresher.pendingIdentity,
		Refresh: func(context.Context) error {
			return refresher.refresh()
		},
		Call: func(callCtx context.Context, id, method string, params json.RawMessage) (json.RawMessage, error) {
			if live != nil {
				result, callErr := callLiveConnectorTool(callCtx, live, id, method, params)
				refresher.observeDaemon(connectorDaemonReleaseIdentity(callCtx))
				return result, callErr
			}
			callReport := report
			if resolveReport != nil {
				var confirmed bool
				callReport, confirmed = resolveReport(callCtx)
				if !confirmed {
					return nil, errors.New("live session identity is not confirmed by the product")
				}
			}
			sourceID := connectorRelaySource(product, callReport.UUID, os.Getenv(launcher.QwenEventsFileEnv))
			result, releaseIdentity, callErr := callConnectorDaemonTool(callCtx, product, sourceID, id, method, params, refresher.identity())
			refresher.observeDaemon(releaseIdentity)
			return result, callErr
		},
	})
	if err != nil {
		return err
	}
	return relay.Serve(ctx, os.Stdin, output)
}

func connectorBeforePublish(refresher *connectorImageRefresher, resolve func(context.Context) string, log io.Writer) func(context.Context) error {
	return func(ctx context.Context) error {
		identity := resolve(ctx)
		if !validConnectorReleaseIdentity(identity) {
			err := errors.New("ready daemon release identity is unavailable")
			_, _ = fmt.Fprintf(log, "Agent Sessions connector refresh: %v\n", err)
			return err
		}
		refresher.observeDaemon(identity)
		if refresher.pendingIdentity() != "" {
			err := errors.New("connector image is stale; retry after automatic refresh")
			_, _ = fmt.Fprintf(log, "Agent Sessions connector refresh: %v\n", err)
			return err
		}
		return nil
	}
}

func callLiveConnectorTool(
	ctx context.Context, live *livepresence.Client, requestID, method string, params json.RawMessage,
) (json.RawMessage, error) {
	if method != "tools/call" {
		return nil, fmt.Errorf("connector method %s is unsupported", method)
	}
	var call connectorToolCall
	if json.Unmarshal(params, &call) != nil || strings.TrimSpace(call.Name) == "" {
		return nil, errors.New("connector tool call is invalid")
	}
	arguments := call.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	var operation string
	switch call.Name {
	case "list_peers":
		operation = "peers.list"
		arguments = map[string]any{}
	case "send_message":
		operation = "message.send"
		arguments = liveMessageSendArguments(arguments)
	case "lane":
		command := strings.TrimSpace(mapString(arguments, "command"))
		operation = "lane." + command
		delete(arguments, "command")
		delete(arguments, "session_id")
		if _, present := arguments["arguments"]; !present {
			arguments["arguments"] = []string{}
		}
	default:
		return nil, livepresence.NewError(livepresence.NotPermitted, "Operation not permitted", map[string]any{"tool": call.Name})
	}
	result, err := live.Call(ctx, requestID, operation, arguments)
	if err != nil {
		return nil, err
	}
	var structured any = map[string]any{}
	if len(result) != 0 && string(result) != "null" && json.Unmarshal(result, &structured) != nil {
		return nil, errors.New("presence method returned invalid JSON")
	}
	return json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(result)}}, "structuredContent": structured,
	})
}

func connectorDaemonReleaseIdentity(ctx context.Context) string {
	response, err := callExistingDaemon(ctx, defaultStateRoot(), daemonpkg.ControlRequest{
		ID: commandRequestID(), Role: daemonpkg.RoleAdmin, Operation: "status",
	})
	if err != nil || response.Error != nil {
		return ""
	}
	return response.ReleaseIdentity
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
	params json.RawMessage, connectorIdentity string,
) (json.RawMessage, string, error) {
	if method != "tools/call" {
		return nil, "", fmt.Errorf("connector method %s is unsupported", method)
	}
	var call connectorToolCall
	if json.Unmarshal(params, &call) != nil || strings.TrimSpace(call.Name) == "" {
		return nil, "", fmt.Errorf("connector tool call is invalid")
	}
	sourceID = connectorToolSource(product, sourceID, params, call.Arguments)
	payload, err := json.Marshal(connectorToolEnvelope{
		SourceID: sourceID, RequestID: requestID, Name: call.Name, Arguments: call.Arguments,
	})
	if err != nil {
		return nil, "", err
	}
	id := commandRequestID()
	response, err := callExistingDaemon(ctx, defaultStateRoot(), daemonpkg.ControlRequest{
		ID: id, Role: daemonpkg.RoleLauncher, Operation: "connector.tool",
		Payload: payload, ConnectorIdentity: connectorIdentity,
	})
	if err != nil {
		return nil, "", err
	}
	if response.Error != nil {
		if response.Error.RPCCode != 0 {
			return nil, response.ReleaseIdentity, &livepresence.RPCError{
				Code: response.Error.RPCCode, Message: response.Error.Message,
				Data: append(json.RawMessage(nil), response.Error.RPCData...),
			}
		}
		if response.Error.Code == daemonpkg.ErrorStaleConnector {
			data, _ := json.Marshal(map[string]string{"reason": daemonpkg.ErrorStaleConnector, "release_identity": response.ReleaseIdentity})
			return nil, response.ReleaseIdentity, &livepresence.RPCError{Code: livepresence.NotPermitted, Message: response.Error.Message, Data: data}
		}
		return nil, response.ReleaseIdentity, errors.New(response.Error.Message)
	}
	return append(json.RawMessage(nil), response.Payload...), response.ReleaseIdentity, nil
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

func connectorNativeCall(ctx context.Context, report livepresence.Report, method string, params json.RawMessage) (json.RawMessage, error) {
	if method != "message.deliver" {
		return nil, fmt.Errorf("live session method %s is unsupported", method)
	}
	message, err := livepresence.DecodeDeliver(params)
	if err != nil {
		return nil, err
	}
	body, err := sessiontools.RenderNativeMessage(message)
	if err != nil {
		return nil, livepresence.NewError(livepresence.InvalidParams, "Invalid params", map[string]any{"method": method})
	}
	adapter := connectorNativeAdapters[report.Product]
	if adapter == nil {
		return nil, fmt.Errorf("product %s has no Go connector delivery adapter", report.Product)
	}
	if err := adapter(ctx, report, message.ID, body); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

type connectorNativeAdapter func(context.Context, livepresence.Report, string, string) error

const (
	connectorProductCodex  = "codex"
	connectorProductClaude = "claude"
	connectorProductQwen   = "qwen"
	connectorProductGrok   = "grok"
)

var connectorNativeAdapters = map[string]connectorNativeAdapter{
	connectorProductCodex:  deliverCodexConnectorMessage,
	connectorProductClaude: deliverClaudeConnectorMessage,
	connectorProductGrok:   deliverGrokConnectorMessage,
}

// Products with a native presence adapter already own the session's one live
// stream. A project-discovered MCP connector may expose the same tool name,
// but it must never open a second connection and replace the product report.
func connectorOwnsLivePresence(product string) bool {
	return product == connectorProductClaude || product == connectorProductGrok
}

func connectorClaimsLivePresence(requestedProduct, product string, getenv func(string) string) bool {
	if requestedProduct == "auto" {
		return false
	}
	managedProduct := strings.TrimSpace(getenv("AGENT_SESSIONS_PRODUCT"))
	if managedProduct != "" && managedProduct != product {
		return false
	}
	return connectorOwnsLivePresence(product)
}

func liveMessageSendArguments(arguments map[string]any) map[string]any {
	projected := map[string]any{"message": mapString(arguments, "message")}
	if target := mapString(arguments, "target"); target != "" {
		projected["target"] = target
	}
	if targets := mapStringSlice(arguments, "targets"); len(targets) > 0 {
		projected["targets"] = targets
	}
	if group := mapString(arguments, "group"); group != "" {
		projected["group"] = group
	}
	return projected
}

func deliverCodexConnectorMessage(ctx context.Context, report livepresence.Report, _ string, body string) error {
	native, err := bridge.OpenCodexNative(ctx, bridge.CodexNativeConfig{
		CodexBinary: codexBinary(), CodexHome: codexHome(),
	})
	if err != nil {
		return err
	}
	defer native.Close()
	_, err = native.SendMessage(ctx, report.UUID, body)
	return err
}

func deliverGrokConnectorMessage(ctx context.Context, report livepresence.Report, messageID, body string) error {
	observer, err := openGrokConnectorObserver(ctx, report)
	if err != nil {
		return err
	}
	defer observer.Close()
	return observer.Interject(ctx, messageID, body)
}

func openGrokConnectorObserver(ctx context.Context, report livepresence.Report) (*bridge.GrokNativeObserver, error) {
	executable, err := launcher.ResolveProductExecutable(connectorProductGrok)
	if err != nil {
		return nil, err
	}
	cwd := strings.TrimSpace(report.Info["cwd"])
	if cwd == "" {
		return nil, errors.New("grok connector working directory is unavailable")
	}
	return bridge.OpenGrokNativeObserver(
		ctx, executable, cwd, strings.TrimSpace(os.Getenv(launcher.GrokLeaderSocketEnv)),
		report.UUID, os.Environ(), os.Stderr,
	)
}

func deliverClaudeConnectorMessage(_ context.Context, _ livepresence.Report, messageID, body string) error {
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

func connectorLiveReport(product string, getenv func(string) string) (livepresence.Report, bool) {
	report, ok := connectorReportFields(product, getenv)
	if !ok {
		return livepresence.Report{}, false
	}
	uuid := ""
	if product == connectorProductGrok {
		uuid = strings.TrimSpace(getenv("GROK_SESSION_ID"))
	}
	productSessionEnv := map[string]string{
		laneCandidateCodex: "CODEX_THREAD_ID", laneCandidateClaude: "CLAUDE_CODE_SESSION_ID", connectorProductGrok: "AGENT_SESSIONS_GROK_SESSION_ID",
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
		return livepresence.Report{}, false
	}
	report.UUID = uuid
	return report, true
}

func connectorReportFields(product string, getenv func(string) string) (livepresence.Report, bool) {
	var groups []string
	if encoded := strings.TrimSpace(getenv("AGENT_SESSIONS_GROUPS")); encoded != "" {
		if json.Unmarshal([]byte(encoded), &groups) != nil {
			return livepresence.Report{}, false
		}
	}
	return livepresence.Report{
		Name: strings.TrimSpace(getenv("AGENT_SESSIONS_SESSION_NAME")), Groups: append([]string(nil), groups...),
		Product: product, Info: map[string]string{},
	}, true
}

func claudeActiveSessionForParent(ctx context.Context, parentPID int, base livepresence.Report) (livepresence.Report, bool) {
	executable, err := launcher.ResolveProductExecutable(connectorProductClaude)
	if err != nil {
		return livepresence.Report{}, false
	}
	payload, err := exec.CommandContext(ctx, executable, "agents", "--json").Output() //nolint:gosec // selected native Claude executable.
	if err != nil {
		return livepresence.Report{}, false
	}
	return claudeActiveSessionForParentFromJSON(payload, parentPID, base)
}

func claudeActiveSessionForParentFromJSON(payload []byte, parentPID int, base livepresence.Report) (livepresence.Report, bool) {
	var sessions []struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Cwd       string `json:"cwd"`
		PID       int    `json:"pid"`
	}
	if json.Unmarshal(payload, &sessions) != nil {
		return livepresence.Report{}, false
	}
	var confirmed livepresence.Report
	for _, session := range sessions {
		sessionID := strings.TrimSpace(session.SessionID)
		if session.PID != parentPID || session.Kind != "interactive" || sessionID == "" {
			continue
		}
		candidate := livepresence.CloneReport(base)
		candidate.UUID = sessionID
		candidate.Name = strings.TrimSpace(session.Name)
		candidate.Info = livepresence.CwdInfo(strings.TrimSpace(session.Cwd))
		if confirmed.UUID != "" && (confirmed.UUID != candidate.UUID || confirmed.Name != candidate.Name || confirmed.Info["cwd"] != candidate.Info["cwd"]) {
			return livepresence.Report{}, false
		}
		confirmed = candidate
	}
	return confirmed, confirmed.UUID != ""
}

// resolveConnectorProduct keeps an automatically discovered connector on the
// managed product's own tools and identity projection. Automatic connectors
// remain tools-only; they never compete with the product's presence owner.
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
		return product, nil
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
