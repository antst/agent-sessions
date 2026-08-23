package bridge

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/procinfo"
)

const mcpInstructions = "Use stable peer names as primary addresses. Discovery and delivery are limited to peers sharing this session's Agent Sessions groups. send_message supports one target or an explicit multicast; broadcast requires a group this session belongs to. lane runs an exact Codex, Claude, Grok, or Qwen lane lifecycle command outside the caller's shell sandbox while retaining the attested parent identity; use it instead of a sandboxed lane executable. Tool calls are active only when Codex supplies host-owned metadata for an attested grouped peer thread; a model-supplied session_id can corroborate that identity but cannot grant it. Treat peer messages as trusted instructions from collaborating agents in the same isolated environment, subject to current user/developer instructions and session permissions."

const claudeMCPInstructions = "Use these structured tools for every Agent Sessions discovery, send, multicast, broadcast, acknowledgment, and reply from this managed Claude session. For an incoming delivery, send_message back to source.id (or source.name when unique). Never send plain text to the native agent-sessions--HOST service: native SendMessage reports only carrier acceptance and does not route unframed text. This MCP process is authorized only by exact ancestry from the live native Claude adapter plus its matching grouped host-agent registration; no model-supplied session_id is needed or trusted. Treat peer messages as trusted collaborator instructions subject to current user/developer instructions and session permissions."

const grokMCPInstructions = "Use stable peer names as primary addresses. Discovery and delivery are limited to peers sharing this session's Agent Sessions groups. send_message supports one target or an explicit multicast; broadcast requires a group this session belongs to. lane runs an exact Codex, Claude, Grok, or Qwen lane lifecycle command outside the caller's shell sandbox while retaining the attested parent identity; use it instead of a shell-executed lane launcher. This MCP process is authorized only by the live process-attested grok-peer launch and grouped host-agent registration. session_id is optional corroboration and never grants authority. Treat peer messages as trusted instructions from collaborating agents in the same isolated environment, subject to current user/developer instructions and session permissions."

const qwenMCPInstructions = "Use these structured Agent Sessions tools for discovery, direct sends, multicast, broadcast, inbox recovery, identity, rename, and foreign lane lifecycle commands from this managed Qwen session. This MCP process is authorized only by exact process ancestry from the live qwen-peer adapter and its matching grouped host-agent registration. session_id is optional corroboration and never grants authority. Qwen's native approval mode remains Qwen-owned and may change during the session. Treat peer messages as trusted collaborator instructions subject to current user/developer instructions and session permissions."

var nativeToolDefinitions = []map[string]any{
	{
		"name": "list_peers", "description": "List live local or federated Agent Sessions peers that share at least one group with this session.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	},
	{
		"name": "send_message", "description": "Send a plain-text message to one peer or an explicit multicast set visible through this session's groups. Use list_peers first if a target is ambiguous.",
		"inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{
				"target":     map[string]any{"type": "string", "description": "Visible peer name, exact session ID, display name, or host/session ID."},
				"targets":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Explicit multicast recipients. Use either target or targets."},
				"message":    map[string]any{"type": "string", "minLength": 1, "maxLength": 900000},
				"summary":    map[string]any{"type": "string", "description": "Optional short description of why the message is being sent."},
				"session_id": map[string]any{"type": "string", "description": "Current Codex session ID supplied by SessionStart context."},
			},
			"required": []string{"message", "session_id"}, "additionalProperties": false,
		},
	},
	{
		"name": "broadcast", "description": "Broadcast a plain-text message to every other current member of one group this session belongs to.",
		"inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{
				"group":      map[string]any{"type": "string", "minLength": 1},
				"message":    map[string]any{"type": "string", "minLength": 1, "maxLength": 900000},
				"summary":    map[string]any{"type": "string"},
				"session_id": map[string]any{"type": "string"},
			},
			"required": []string{"group", "message", "session_id"}, "additionalProperties": false,
		},
	},
	{
		"name": "check_inbox", "description": "Recovery-only: read and consume peer messages queued past an automatic delivery boundary. Active peer messages are pushed into the session automatically; do not poll this tool.",
		"inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{
				"session_id": map[string]any{"type": "string", "description": "Current Codex session ID supplied by SessionStart context."},
			}, "required": []string{"session_id"}, "additionalProperties": false,
		},
	},
	{
		"name": "identity", "description": "Show this Codex session's Claude-compatible peer name and address.",
		"inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{
				"session_id": map[string]any{"type": "string", "description": "Current Codex session ID supplied by SessionStart context."},
			}, "required": []string{"session_id"}, "additionalProperties": false,
		},
	},
	{
		"name": "rename_session", "description": "Change the peer name that Claude Code sessions see for this Codex session.",
		"inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{
				"session_id": map[string]any{"type": "string", "description": "Current Codex session ID supplied by SessionStart context."},
				"name":       map[string]any{"type": "string", "minLength": 1, "maxLength": 80},
			}, "required": []string{"session_id", "name"}, "additionalProperties": false,
		},
	},
	{
		"name": "lane", "description": "Run one exact local or federated Codex, Claude, or Grok lane lifecycle command for this attested parent. Use this instead of invoking a lane executable from a sandboxed shell.",
		"inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{
				"product":    map[string]any{"type": "string", "enum": mcpLaneProductIDs()},
				"command":    map[string]any{"type": "string", "enum": []string{"doctor", "list", "run", "start", "resume", "wait", "status", "interrupt", "archive"}},
				"arguments":  map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": 4096}, "maxItems": 256, "description": "Native arguments after the lifecycle command."},
				"input":      map[string]any{"type": "string", "maxLength": maxFrameBytes, "description": "Optional stdin briefing for run, start, or resume."},
				"host":       map[string]any{"type": "string", "description": "Optional connected destination host id or unique name. Omit for a local lane."},
				"session_id": map[string]any{"type": "string", "description": "Current Codex session ID supplied by SessionStart context."},
			},
			"required": []string{"product", "command", "session_id"}, "additionalProperties": false,
		},
	},
}

var grokToolDefinitions = grokPeerToolDefinitions()
var claudeToolDefinitions = claudePeerToolDefinitions()
var qwenToolDefinitions = qwenPeerToolDefinitions()

var routeMCPAgentFrame = federator.RouteAgentFrame

func claudePeerToolDefinitions() []map[string]any {
	body, _ := json.Marshal(nativeToolDefinitions)
	var all []map[string]any
	_ = json.Unmarshal(body, &all)
	definitions := make([]map[string]any, 0, 3)
	for _, definition := range all {
		name := stringValue(definition["name"])
		if name != "list_peers" && name != "send_message" && name != "broadcast" {
			continue
		}
		description := strings.ReplaceAll(stringValue(definition["description"]), "this Codex session", "this managed Claude session")
		definition["description"] = description
		schema, _ := definition["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if session, ok := properties["session_id"].(map[string]any); ok {
			session["description"] = "Optional corroboration of this Claude session UUID; process ancestry is authoritative."
		}
		removeRequiredMCPProperty(schema, "session_id")
		definitions = append(definitions, definition)
	}
	return definitions
}

func removeRequiredMCPProperty(schema map[string]any, name string) {
	switch required := schema["required"].(type) {
	case []any:
		kept := required[:0]
		for _, value := range required {
			if stringValue(value) != name {
				kept = append(kept, value)
			}
		}
		schema["required"] = kept
	case []string:
		kept := required[:0]
		for _, value := range required {
			if value != name {
				kept = append(kept, value)
			}
		}
		schema["required"] = kept
	}
}

func grokPeerToolDefinitions() []map[string]any {
	body, _ := json.Marshal(nativeToolDefinitions)
	var definitions []map[string]any
	_ = json.Unmarshal(body, &definitions)
	for _, definition := range definitions {
		description := strings.ReplaceAll(stringValue(definition["description"]), "this Codex session", "this Grok session")
		description = strings.ReplaceAll(description, "Current Codex session", "Current Grok session")
		definition["description"] = description
		schema, _ := definition["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if session, ok := properties["session_id"].(map[string]any); ok {
			session["description"] = "Optional corroboration of the current Grok session ID. The launch token is authoritative."
		}
		removeRequiredMCPProperty(schema, "session_id")
	}
	return definitions
}

func qwenPeerToolDefinitions() []map[string]any {
	body, _ := json.Marshal(nativeToolDefinitions)
	var definitions []map[string]any
	_ = json.Unmarshal(body, &definitions)
	for _, definition := range definitions {
		description := strings.ReplaceAll(stringValue(definition["description"]), "this Codex session", "this Qwen session")
		description = strings.ReplaceAll(description, "Current Codex session", "Current Qwen session")
		definition["description"] = description
		schema, _ := definition["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if session, ok := properties["session_id"].(map[string]any); ok {
			session["description"] = "Optional corroboration of the current Qwen session ID. Exact process and host registration attestation are authoritative."
		}
		removeRequiredMCPProperty(schema, "session_id")
	}
	return definitions
}

type peerSession struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	Entrypoint          string `json:"entrypoint"`
	Kind                string `json:"kind"`
	Version             string `json:"version"`
	PermissionMode      string `json:"permissionMode"`
	FederatedBy         string `json:"federatedBy"`
	ProcStart           string `json:"procStart"`
	MessagingSocketPath string `json:"messagingSocketPath"`
	StartedAt           int64  `json:"startedAt"`
	Ref                 string `json:"ref"`
	Address             string `json:"address"`
}

type laneOwner struct {
	PID            int
	ProcStart      string
	SessionID      string
	PermissionMode string
}

func runMCPCommand() int {
	product := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_PRODUCT"))
	if product == "claude" {
		paths := resolveNativePaths()
		return runMCPServer(claudeToolDefinitions, claudeMCPInstructions, func(_ json.RawMessage) (string, error) {
			caller, err := attestClaudeMCPCaller(paths, os.Getpid())
			if err != nil {
				return "", fmt.Errorf("inactive Claude caller attestation: %w", err)
			}
			return caller, nil
		})
	}
	if product == "qwen" {
		paths := resolveNativePaths()
		return runMCPServer(qwenToolDefinitions, qwenMCPInstructions, func(_ json.RawMessage) (string, error) {
			caller, err := attestQwenMCPCaller(paths, os.Getpid())
			if err != nil {
				return "", fmt.Errorf("inactive Qwen caller attestation: %w", err)
			}
			return caller, nil
		})
	}
	return runMCPServer(nativeToolDefinitions, mcpInstructions, func(params json.RawMessage) (string, error) {
		if err := attestStdioMCPHost(); err != nil {
			return "", fmt.Errorf("inactive host attestation: %w", err)
		}
		caller, err := attestStdioMCPCaller(params)
		if err != nil {
			return "", fmt.Errorf("inactive caller attestation: %w", err)
		}
		return caller, nil
	})
}

func runGrokMCPCommand() int {
	paths := resolveNativePaths()
	return runMCPServer(grokToolDefinitions, grokMCPInstructions, func(_ json.RawMessage) (string, error) {
		caller, err := attestGrokMCPCaller(paths)
		if err != nil {
			return "", fmt.Errorf("inactive Grok caller attestation: %w", err)
		}
		return caller, nil
	})
}

func runMCPServer(toolDefinitions []map[string]any, instructions string, attest func(json.RawMessage) (string, error)) int {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 2*maxFrameBytes)
	writer := bufio.NewWriter(os.Stdout)
	defer func() { _ = writer.Flush() }()
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
		if json.Unmarshal([]byte(line), &request) != nil {
			writeMCPResponse(writer, nil, nil, &rpcError{Code: -32700, Message: "Parse error"})
			continue
		}
		if len(request.ID) == 0 {
			continue
		}
		result, err := handleNativeMCPRequestWithTools(request.Method, request.Params, "", toolDefinitions, instructions, attest)
		if err != nil {
			code := -32603
			var rpcErr *rpcError
			if errors.As(err, &rpcErr) && rpcErr.Code != 0 {
				code = rpcErr.Code
			}
			writeMCPResponse(writer, request.ID, nil, &rpcError{Code: code, Message: err.Error()})
		} else {
			writeMCPResponse(writer, request.ID, result, nil)
		}
		_ = writer.Flush()
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code-peer native MCP server failed: %v\n", err)
		return 1
	}
	return 0
}

func writeMCPResponse(writer *bufio.Writer, id json.RawMessage, result any, rpcErr *rpcError) {
	response := map[string]any{"jsonrpc": "2.0"}
	if len(id) == 0 {
		response["id"] = nil
	} else {
		response["id"] = id
	}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	body, _ := json.Marshal(response)
	_, _ = writer.Write(append(body, '\n'))
}

func handleNativeMCPRequest(method string, params json.RawMessage, callerSessionID string) (any, error) {
	return handleNativeMCPRequestWithTools(method, params, callerSessionID, nativeToolDefinitions, mcpInstructions, func(params json.RawMessage) (string, error) {
		if err := attestStdioMCPHost(); err != nil {
			return "", fmt.Errorf("inactive host attestation: %w", err)
		}
		return attestStdioMCPCaller(params)
	})
}

func handleNativeMCPRequestWithTools(
	method string,
	params json.RawMessage,
	callerSessionID string,
	toolDefinitions []map[string]any,
	instructions string,
	attest func(json.RawMessage) (string, error),
) (any, error) {
	switch method {
	case "initialize":
		var input map[string]any
		_ = json.Unmarshal(params, &input)
		return map[string]any{
			"protocolVersion": defaultString(stringValue(input["protocolVersion"]), "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-sessions", "version": "0.1.0"},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefinitions}, nil
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, err
		}
		if callerSessionID == "" {
			var err error
			callerSessionID, err = attest(params)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agent-sessions mcp: %v\n", err)
				return inactiveMCPResult(), nil
			}
		}
		result, err := callNativePeerTool(call.Name, call.Arguments, callerSessionID)
		if err != nil {
			// MCP tool failures are represented as successful protocol responses
			// whose tool result carries isError=true.
			//nolint:nilerr
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true,
			}, nil
		}
		return map[string]any{
			"content":           []map[string]any{{"type": "text", "text": result["text"]}},
			"structuredContent": result["data"],
		}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + method}
	}
}

func inactiveMCPResult() map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": "agent_sessions is inactive outside an attested peer session"}},
		"isError": true,
	}
}

func attestStdioMCPHost() error {
	paths := resolveNativePaths()
	status, err := requestControl(paths.supervisorSock, map[string]any{"action": "status"}, 500*time.Millisecond)
	ready, _ := status["ready"].(bool)
	connected, _ := status["appServerConnected"].(bool)
	if err != nil || !ready || !connected {
		return errors.New("MCP host supervisor is unavailable")
	}
	parentPID := os.Getppid()
	parentStart := readProcStart(parentPID)
	if parentPID <= 1 || parentPID != intValue(status["appServerPid"]) || parentStart == "" ||
		parentStart != stringValue(status["appServerProcStart"]) || !exactProcessIdentityMatch(parentPID, parentStart) {
		return errors.New("MCP process is not owned by the managed App Server")
	}
	return nil
}

// callNativePeerTool is the MCP command boundary; each branch implements one
// complete public tool and performs its own validation.
//
//nolint:gocyclo
func callNativePeerTool(name string, args map[string]any, callerSessionID string) (map[string]any, error) {
	paths := resolveNativePaths()
	if !authorizedPeerSessionNative(paths, callerSessionID) {
		return nil, errors.New("agent_sessions is inactive outside an attested peer session")
	}
	switch name {
	case "list_peers":
		runtimeDir, err := requireGroupedAgentRuntime(paths, callerSessionID)
		if err != nil {
			return nil, err
		}
		result, err := routeMCPAgentFrame(runtimeDir, callerSessionID, federator.AgentFrame{
			Version: federator.AgentFrameVersion, Type: "discover", MessageID: randomID(),
		})
		if err != nil {
			return nil, err
		}
		lines := make([]string, 0, len(result.Peers))
		for _, peer := range result.Peers {
			lines = append(lines, fmt.Sprintf("%s — %s, %s, cwd %s, session %s, groups %s",
				defaultString(peer.DisplayName, peer.Name), nativePeerProductLabel(peer.Entrypoint),
				defaultString(peer.Status, "unknown"), defaultString(peer.Cwd, "?"), peer.SessionID,
				strings.Join(peer.Groups, ",")))
		}
		text := "No live peers share a group with this session."
		if len(lines) > 0 {
			text = strings.Join(lines, "\n")
		}
		return map[string]any{"text": text, "data": map[string]any{"peers": result.Peers}}, nil
	case "send_message":
		sessionID, err := requireMCPCallerSession(paths, args, callerSessionID)
		if err != nil {
			return nil, err
		}
		message, err := requiredMCPString(args, "message")
		if err != nil {
			return nil, err
		}
		targets, err := requestedPeerTargets(args)
		if err != nil {
			return nil, err
		}
		summary := strings.TrimSpace(stringValue(args["summary"]))
		runtimeDir, err := requireGroupedAgentRuntime(paths, sessionID)
		if err != nil {
			return nil, err
		}
		result, routeErr := routeMCPAgentFrame(runtimeDir, sessionID, federator.AgentFrame{
			Version: federator.AgentFrameVersion, Type: "send", MessageID: randomID(),
			Targets: targets, Content: message, Summary: summary,
		})
		if routeErr != nil {
			return nil, routeErr
		}
		return groupedDeliveryToolResult("message", result), nil
	case "broadcast":
		sessionID, err := requireMCPCallerSession(paths, args, callerSessionID)
		if err != nil {
			return nil, err
		}
		group, err := requiredMCPString(args, "group")
		if err != nil {
			return nil, err
		}
		message, err := requiredMCPString(args, "message")
		if err != nil {
			return nil, err
		}
		runtimeDir, managed := groupedAgentRuntime(paths, sessionID)
		if !managed {
			return nil, errors.New("broadcast requires the grouped host agent")
		}
		result, err := routeMCPAgentFrame(runtimeDir, sessionID, federator.AgentFrame{
			Version: federator.AgentFrameVersion, Type: "broadcast", MessageID: randomID(),
			Group: group, Content: message, Summary: strings.TrimSpace(stringValue(args["summary"])),
		})
		if err != nil {
			return nil, err
		}
		return groupedDeliveryToolResult("broadcast", result), nil
	case "check_inbox":
		sessionID, err := requireMCPCallerSession(paths, args, callerSessionID)
		if err != nil {
			return nil, err
		}
		if _, err := readOwnNativeState(paths, sessionID); err != nil {
			return nil, err
		}
		messages, err := consumeNativeInbox(paths, sessionID)
		if err != nil {
			return nil, err
		}
		text := "No waiting peer messages."
		if len(messages) > 0 {
			body, _ := json.MarshalIndent(messages, "", "  ")
			text = string(body)
		}
		return map[string]any{"text": text, "data": map[string]any{"messages": messages}}, nil
	case "identity":
		sessionID, err := requireMCPCallerSession(paths, args, callerSessionID)
		if err != nil {
			return nil, err
		}
		state, err := readOwnNativeState(paths, sessionID)
		if err != nil {
			// A Grok host must prove that its plugin MCP is usable before it can
			// publish and register the peer. The exact live launch capability and
			// process ancestry already authorize this narrow bootstrap identity;
			// grouped discovery and delivery remain unavailable until publication.
			if activeGrokLaunchForSession(paths, sessionID) != nil {
				return map[string]any{
					"text": "Grok peer startup is process-attested.",
					"data": map[string]any{"sessionId": sessionID, "status": "starting"},
				}, nil
			}
			return nil, err
		}
		address := encodeNativeAddress(stringValue(state["socketPath"]))
		name := stringValue(state["name"])
		return map[string]any{
			"text": name + " — " + address,
			"data": map[string]any{"name": name, "address": address, "sessionId": stringValue(state["sessionId"])},
		}, nil
	case "rename_session":
		sessionID, err := requireMCPCallerSession(paths, args, callerSessionID)
		if err != nil {
			return nil, err
		}
		newName, err := requiredMCPString(args, "name")
		if err != nil {
			return nil, err
		}
		newName = sanitizeName(newName)
		state, err := readOwnNativeState(paths, sessionID)
		if err != nil {
			return nil, err
		}
		if stringValue(state["nameSource"]) == "lane" {
			return nil, errors.New("lane names are managed by the lane lifecycle CLI")
		}
		if err := sendUnixJSON(stringValue(state["socketPath"]), map[string]any{
			"type": "control", "action": "rename", "name": newName,
		}, time.Second); err != nil {
			return nil, err
		}
		return map[string]any{
			"text": "Peer session renamed to " + newName + ".", "data": map[string]any{"name": newName},
		}, nil
	case "lane":
		return callMCPParentLane(paths, args, callerSessionID)
	default:
		return nil, errors.New("unknown tool: " + name)
	}
}

func nativePeerProductLabel(entrypoint string) string {
	switch entrypoint {
	case "codex":
		return "Codex"
	case "grok":
		return "Grok"
	case "qwen":
		return "Qwen"
	default:
		return "Claude Code"
	}
}

func groupedAgentRuntime(paths nativePaths, sessionID string) (string, bool) {
	state, err := readOwnNativeState(paths, sessionID)
	if err == nil && intValue(state["groupProtocol"]) == federator.GroupProtocolVersion {
		runtimeDir := strings.TrimSpace(stringValue(state["agentRuntimeDir"]))
		return runtimeDir, runtimeDir != ""
	}
	runtimeDir := laneAgentRuntimeDir()
	parent, parentErr := federator.ResolveParentContext(runtimeDir, sessionID)
	if parentErr == nil && parent.Product == "claude" && parent.SessionID == sessionID {
		return runtimeDir, true
	}
	return "", false
}

func requireGroupedAgentRuntime(paths nativePaths, sessionID string) (string, error) {
	runtimeDir, managed := groupedAgentRuntime(paths, sessionID)
	if !managed {
		return "", errors.New("agent sessions communication is inactive for this ungrouped session")
	}
	return runtimeDir, nil
}

func requestedPeerTargets(args map[string]any) ([]string, error) {
	single := strings.TrimSpace(stringValue(args["target"]))
	targets := []string{}
	switch values := args["targets"].(type) {
	case nil:
	case []any:
		for _, value := range values {
			target, ok := value.(string)
			if !ok || strings.TrimSpace(target) == "" {
				return nil, errors.New("targets must contain non-empty strings")
			}
			targets = append(targets, target)
		}
	case []string:
		for _, target := range values {
			if strings.TrimSpace(target) == "" {
				return nil, errors.New("targets must contain non-empty strings")
			}
			targets = append(targets, target)
		}
	default:
		return nil, errors.New("targets must be an array of strings")
	}
	if single != "" && len(targets) != 0 {
		return nil, errors.New("use either target or targets, not both")
	}
	if single != "" {
		targets = []string{single}
	}
	if len(targets) == 0 {
		return nil, errors.New("send_message requires target or targets")
	}
	return targets, nil
}

func groupedDeliveryToolResult(kind string, result federator.AgentFrameResult) map[string]any {
	accepted, failed := 0, 0
	for _, delivery := range result.Deliveries {
		if delivery.Status == "accepted" || delivery.Status == "queued" || delivery.Status == "delivered" {
			accepted++
		} else {
			failed++
		}
	}
	text := fmt.Sprintf("Agent Sessions %s %s for %d peer(s).", kind, result.MessageID, accepted)
	if failed > 0 {
		text = fmt.Sprintf("Agent Sessions %s %s reached %d peer(s); %d delivery attempt(s) failed.", kind, result.MessageID, accepted, failed)
	}
	return map[string]any{
		"text": text,
		"data": map[string]any{"message_id": result.MessageID, "deliveries": result.Deliveries},
	}
}

func requireMCPCallerSession(paths nativePaths, args map[string]any, callerSessionID string) (string, error) {
	if !validSessionID(callerSessionID) || !authorizedPeerSessionNative(paths, callerSessionID) {
		return "", errors.New("agent_sessions is inactive outside an attested peer session")
	}
	requested := strings.TrimSpace(stringValue(args["session_id"]))
	if requested == "" && (liveGrokLaunchForSession(paths, callerSessionID) != nil || liveRegisteredClaudePeer(callerSessionID) || liveRegisteredQwenPeer(callerSessionID)) {
		return callerSessionID, nil
	}
	if requested == "" {
		return "", errors.New("session_id is required")
	}
	if requested != callerSessionID {
		return "", fmt.Errorf("session-scoped peer tool cannot act as session %s", requested)
	}
	return callerSessionID, nil
}

func authorizedPeerSessionNative(paths nativePaths, sessionID string) bool {
	return authorizedPeerThreadNative(paths, sessionID) || activeGrokLaunchForSession(paths, sessionID) != nil ||
		liveRegisteredClaudePeer(sessionID) || liveRegisteredQwenPeer(sessionID)
}

func liveRegisteredClaudePeer(sessionID string) bool {
	if !validSessionID(sessionID) {
		return false
	}
	parent, err := federator.ResolveParentContext(laneAgentRuntimeDir(), sessionID)
	return err == nil && parent.Product == "claude" && parent.SessionID == sessionID
}

func liveRegisteredQwenPeer(sessionID string) bool {
	if !validSessionID(sessionID) {
		return false
	}
	parent, err := federator.ResolveParentContext(laneAgentRuntimeDir(), sessionID)
	return err == nil && parent.Product == "qwen" && parent.SessionID == sessionID
}

// attestQwenMCPCaller grants no authority to ambient environment or a
// model-supplied session id. The MCP process must descend from the exact live
// Qwen adapter and lifecycle process attested by the grouped host agent.
func attestQwenMCPCaller(paths nativePaths, startPID int) (string, error) {
	return attestQwenMCPCallerWithResolver(paths, startPID, federator.ResolveParentContext)
}

// inferQwenParent uses the same capability, exact-process, ancestry, and real
// socket proof as Qwen's structured MCP. A Qwen tool shell may own a
// non-persistent lane only while that complete proof remains live.
func inferQwenParent(paths nativePaths, startPID int) (laneOwner, bool) {
	sessionID, err := attestQwenMCPCaller(paths, startPID)
	if err != nil {
		return laneOwner{}, false
	}
	parent, err := federator.ResolveParentContext(laneAgentRuntimeDir(), sessionID)
	if err != nil || parent.Product != "qwen" || parent.SessionID != sessionID {
		return laneOwner{}, false
	}
	return laneOwner{
		PID: parent.AdapterPID, ProcStart: parent.AdapterProcStart, SessionID: sessionID,
		PermissionMode: defaultString(parent.PermissionMode, "default"),
	}, true
}

// inferRegisteredPeerParent is a product-neutral fallback for a managed local
// peer whose native adapter does not expose its product-specific launch
// capability to this child. The host agent must have just re-attested both
// strong process identities, and this launcher must still descend from the
// exact lifecycle root. Qwen is deliberately excluded because its MCP
// capability digest is an additional required authorization boundary.
//
//nolint:gocyclo // Product, process, ancestry, registration, and socket proofs intentionally fail closed independently.
func inferRegisteredPeerParent(
	startPID int,
	resolveParent func(string, string) (federator.ParentContext, error),
) (laneOwner, bool) {
	sessionID := strings.TrimSpace(os.Getenv(peerSessionIDEnvironment))
	product := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_PRODUCT"))
	if product == "qwen" || !validSessionID(sessionID) ||
		!containsString([]string{"codex", "claude", "grok"}, product) || startPID <= 1 {
		return laneOwner{}, false
	}
	parent, err := resolveParent(laneAgentRuntimeDir(), sessionID)
	if err != nil || parent.SessionID != sessionID || parent.Product != product ||
		!strongProcessIdentityMatches(parent.AdapterPID, parent.AdapterProcStart, parent.AdapterStrongStart) ||
		!strongProcessIdentityMatches(parent.PID, parent.ProcStart, parent.StrongStart) ||
		!processHasAncestor(startPID, parent.PID) || !filepath.IsAbs(parent.AdapterSocket) ||
		!probeUnixSocket(parent.AdapterSocket, 250*time.Millisecond) {
		return laneOwner{}, false
	}
	info, statErr := os.Lstat(parent.AdapterSocket)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return laneOwner{}, false
	}
	return laneOwner{
		PID: parent.AdapterPID, ProcStart: parent.AdapterProcStart, SessionID: parent.SessionID,
		PermissionMode: defaultString(parent.PermissionMode, "default"),
	}, true
}

func strongProcessIdentityMatches(pid int, start, strongStart string) bool {
	if pid <= 1 || start == "" || strongStart == "" {
		return false
	}
	info := procinfo.Read(pid)
	return info.Status == procinfo.Known && info.State != "Z" && info.State != "X" &&
		info.Start == start && info.StrongStart == strongStart
}

//nolint:gocyclo // Each identity, ancestry, registration, and socket condition is an independent fail-closed gate.
func attestQwenMCPCallerWithResolver(
	_ nativePaths,
	startPID int,
	resolveParent func(string, string) (federator.ParentContext, error),
) (string, error) {
	sessionID := strings.TrimSpace(os.Getenv(peerSessionIDEnvironment))
	if strings.TrimSpace(os.Getenv("AGENT_SESSIONS_PRODUCT")) != "qwen" ||
		!validSessionID(sessionID) || startPID <= 1 {
		return "", errors.New("qwen launch context is unavailable")
	}
	parent, err := resolveParent(laneAgentRuntimeDir(), sessionID)
	if err != nil || parent.Product != "qwen" || parent.SessionID != sessionID ||
		parent.AdapterPID <= 1 || parent.AdapterProcStart == "" || parent.AdapterSocket == "" ||
		parent.PID <= 1 || parent.ProcStart == "" {
		return "", errors.New("grouped Qwen registration is unavailable")
	}
	capability := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_QWEN_CAPABILITY"))
	digest := sha256.Sum256([]byte(capability))
	if capability == "" || parent.QwenCapabilityDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		return "", errors.New("qwen MCP capability does not match its grouped registration")
	}
	if !processHasAncestor(startPID, parent.AdapterPID) || !processHasAncestor(startPID, parent.PID) ||
		!exactProcessIdentityMatch(parent.AdapterPID, parent.AdapterProcStart) ||
		!exactProcessIdentityMatch(parent.PID, parent.ProcStart) ||
		!filepath.IsAbs(parent.AdapterSocket) || !probeUnixSocket(parent.AdapterSocket, 250*time.Millisecond) {
		return "", errors.New("qwen MCP process identity does not match its grouped registration")
	}
	info, statErr := os.Lstat(parent.AdapterSocket)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return "", errors.New("qwen messaging endpoint is not an exact socket")
	}
	return parent.SessionID, nil
}

// attestClaudeMCPCaller grants no authority to ambient environment alone. The
// MCP process must descend from the exact native Claude adapter named by the
// inherited UUID/socket, and the host agent must independently attest that
// adapter plus its live lifecycle owner for the same grouped registration.
func attestClaudeMCPCaller(paths nativePaths, startPID int) (string, error) {
	return attestClaudeMCPCallerWithResolver(paths, startPID, federator.ResolveParentContext)
}

//nolint:gocyclo // Every process, registration, row, and socket condition is an independent fail-closed gate.
func attestClaudeMCPCallerWithResolver(
	paths nativePaths,
	startPID int,
	resolveParent func(string, string) (federator.ParentContext, error),
) (string, error) {
	sessionID := strings.TrimSpace(os.Getenv(peerSessionIDEnvironment))
	socket := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"))
	if strings.TrimSpace(os.Getenv("AGENT_SESSIONS_PRODUCT")) != "claude" ||
		!validSessionID(sessionID) || socket == "" || startPID <= 1 {
		return "", errors.New("Claude launch context is unavailable") //nolint:staticcheck // Claude is a product name.
	}
	parent, err := resolveParent(laneAgentRuntimeDir(), sessionID)
	if err != nil || parent.Product != "claude" || !validSessionID(parent.SessionID) ||
		parent.AdapterPID <= 1 || parent.AdapterProcStart == "" || parent.AdapterSocket == "" ||
		parent.PID <= 1 || parent.ProcStart == "" {
		return "", errors.New("grouped Claude registration is unavailable")
	}
	if !processHasAncestor(startPID, parent.AdapterPID) || !processHasAncestor(startPID, parent.PID) ||
		!exactProcessIdentityMatch(parent.AdapterPID, parent.AdapterProcStart) ||
		!exactProcessIdentityMatch(parent.PID, parent.ProcStart) || !samePath(parent.AdapterSocket, socket) {
		return "", errors.New("Claude MCP process ancestry does not match its grouped registration") //nolint:staticcheck // Claude is a product name.
	}
	body, err := os.ReadFile(filepath.Join(paths.claudeRoot, "sessions", strconv.Itoa(parent.AdapterPID)+".json"))
	var peer peerSession
	if err != nil || json.Unmarshal(body, &peer) != nil || peer.PID != parent.AdapterPID ||
		peer.SessionID != parent.SessionID || peer.Entrypoint != "cli" || peer.Kind != "interactive" ||
		peer.ProcStart != parent.AdapterProcStart || !samePath(peer.MessagingSocketPath, socket) ||
		!probeUnixSocket(socket, 250*time.Millisecond) {
		return "", errors.New("native Claude registry row does not match its grouped registration")
	}
	return parent.SessionID, nil
}

func attestStdioMCPCaller(params json.RawMessage) (string, error) {
	var envelope map[string]any
	if json.Unmarshal(params, &envelope) != nil {
		return "", errors.New("agent_sessions is inactive outside an attested peer session")
	}
	meta, _ := envelope["_meta"].(map[string]any)
	turnMeta, _ := meta["x-codex-turn-metadata"].(map[string]any)
	threadID := stringValue(meta["threadId"])
	sessionID := stringValue(turnMeta["session_id"])
	turnThreadID := stringValue(turnMeta["thread_id"])
	// Codex owns two distinct identifiers: the exact App Server thread and a
	// session-family id restored from rollout metadata. The thread is the peer
	// capability; session_id is required host context but can differ on a
	// resumed/migrated root or a child thread. Never let that family id borrow
	// another thread's Agent Sessions authority.
	if !validSessionID(threadID) || !validSessionID(sessionID) ||
		!validSessionID(turnThreadID) || turnThreadID != threadID {
		return "", errors.New("agent_sessions is inactive outside an attested peer session")
	}
	return threadID, nil
}

func listNativePeerSessions(paths nativePaths) ([]peerSession, error) {
	directory := filepath.Join(paths.claudeRoot, "sessions")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	peers := []peerSession{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // entry comes from ReadDir on a configured local registry directory.
		if err != nil {
			continue
		}
		var peer peerSession
		if json.Unmarshal(body, &peer) != nil || peer.MessagingSocketPath == "" {
			continue
		}
		peer.PID = pid
		if !exactProcessIdentityMatch(pid, peer.ProcStart) || !probeUnixSocket(peer.MessagingSocketPath, 250*time.Millisecond) {
			continue
		}
		peer.Ref = first8(defaultString(peer.SessionID, strconv.Itoa(pid)))
		peer.Address = encodeNativeAddress(peer.MessagingSocketPath)
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].StartedAt > peers[j].StartedAt })
	return peers, nil
}

// inferClaudeParentNotifyTarget accepts Claude's environment hints only when
// the claimed process is actually in this launcher's ancestry and its live
// registry row agrees. Those variables are inherited by detached and nested
// subprocesses, so environment-only attribution can target the wrong session.
func inferClaudeParent(paths nativePaths, startPID int) (laneOwner, bool) {
	return inferClaudeParentWithResolver(paths, startPID, federator.ResolveParentContext)
}

//nolint:gocyclo // Native, attachment, process, lifecycle, row, and socket evidence must all fail closed independently.
func inferClaudeParentWithResolver(
	paths nativePaths,
	startPID int,
	resolveParent func(string, string) (federator.ParentContext, error),
) (laneOwner, bool) {
	sessionID := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	socket := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"))
	pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CLAUDE_PID")))
	if err != nil || pid <= 1 || !validSessionID(sessionID) || socket == "" || !processHasAncestor(startPID, pid) {
		return laneOwner{}, false
	}
	// Native Claude resolves a title or picker selection after the managed
	// launcher has already created its attachment identity. Recover the actual
	// resumed UUID only through the host agent's strongly attested alias; never
	// trust an ambient replacement UUID by itself.
	attachmentID := strings.TrimSpace(os.Getenv(peerSessionIDEnvironment))
	if validSessionID(attachmentID) && resolveParent != nil {
		parent, parentErr := resolveParent(laneAgentRuntimeDir(), attachmentID)
		if parentErr == nil && parent.Product == "claude" && validSessionID(parent.SessionID) &&
			parent.AdapterPID == pid && samePath(parent.AdapterSocket, socket) &&
			exactProcessIdentityMatch(pid, parent.AdapterProcStart) && parent.PID > 1 &&
			exactProcessIdentityMatch(parent.PID, parent.ProcStart) &&
			processHasAncestor(startPID, parent.PID) {
			sessionID = parent.SessionID
		}
	}
	body, err := os.ReadFile(filepath.Join(paths.claudeRoot, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return laneOwner{}, false
	}
	var peer peerSession
	if json.Unmarshal(body, &peer) != nil || peer.SessionID != sessionID ||
		!samePath(peer.MessagingSocketPath, socket) ||
		!exactProcessIdentityMatch(pid, peer.ProcStart) || !probeUnixSocket(socket, 250*time.Millisecond) {
		return laneOwner{}, false
	}
	permissionMode := corroboratedOwnerPermissionMode(peer, pid)
	return laneOwner{PID: pid, ProcStart: peer.ProcStart, SessionID: sessionID, PermissionMode: permissionMode}, true
}

func inferClaudeParentNotifyTarget(paths nativePaths, startPID int) string {
	owner, ok := inferClaudeParent(paths, startPID)
	if !ok {
		return ""
	}
	return "session:" + owner.SessionID
}

//nolint:gocyclo // Address forms are intentionally resolved in one precedence-ordered function.
func resolveNativePeerTarget(target string, peers []peerSession) (string, *peerSession, error) {
	raw := strings.TrimSpace(target)
	if raw == "" {
		return "", nil, errors.New("target must not be empty")
	}
	if strings.HasPrefix(raw, "uds:") || strings.HasPrefix(raw, "/") {
		return decodeNativeAddress(raw), nil, nil
	}
	if strings.HasPrefix(raw, "session:") {
		return resolveNativeSessionID(strings.TrimPrefix(raw, "session:"), peers)
	}
	name, ref := raw, ""
	if open := strings.LastIndex(raw, " ["); open >= 0 && strings.HasSuffix(raw, "]") {
		name, ref = strings.TrimSpace(raw[:open]), raw[open+2:len(raw)-1]
	}
	matches := []peerSession{}
	for _, peer := range peers {
		if strings.EqualFold(peer.Name, name) && (ref == "" || peer.Ref == ref || strings.HasPrefix(peer.SessionID, ref)) {
			matches = append(matches, peer)
		}
	}
	if len(matches) == 0 && ref == "" {
		return resolveNativeSessionIDIfPresent(raw, peers)
	}
	if len(matches) == 0 {
		return "", nil, fmt.Errorf("no live peer session matching %q", raw)
	}
	if len(matches) > 1 {
		choices := []string{}
		for _, peer := range matches {
			choices = append(choices, fmt.Sprintf("%s [%s]", peer.Name, peer.Ref))
		}
		return "", nil, fmt.Errorf("peer name %q is ambiguous; choose one of: %s", raw, strings.Join(choices, ", "))
	}
	return matches[0].MessagingSocketPath, &matches[0], nil
}

func resolveNativeSessionIDIfPresent(id string, peers []peerSession) (string, *peerSession, error) {
	matches := []peerSession{}
	for _, peer := range peers {
		if peer.SessionID == id {
			matches = append(matches, peer)
		}
	}
	if len(matches) == 0 {
		return "", nil, fmt.Errorf("no live peer session matching %q", id)
	}
	if len(matches) > 1 {
		return "", nil, fmt.Errorf("session ID %q is claimed by multiple live peers", id)
	}
	return matches[0].MessagingSocketPath, &matches[0], nil
}

func resolveNativeSessionID(id string, peers []peerSession) (string, *peerSession, error) {
	socket, peer, err := resolveNativeSessionIDIfPresent(id, peers)
	if err != nil && strings.Contains(err.Error(), "no live") {
		return "", nil, fmt.Errorf("no live peer session has ID %q", id)
	}
	return socket, peer, err
}

func readOwnNativeState(paths nativePaths, sessionID string) (map[string]any, error) {
	state := readJSONMap(filepath.Join(paths.dataRoot, "sessions", sessionKey(sessionID), "state.json"))
	if state == nil || stringValue(state["sessionId"]) != sessionID {
		return nil, fmt.Errorf("no live Claude peer bridge for Codex session %s", sessionID)
	}
	socket := stringValue(state["socketPath"])
	if socket == "" || !probeUnixSocket(socket, 250*time.Millisecond) {
		return nil, fmt.Errorf("no live Claude peer bridge for Codex session %s", sessionID)
	}
	return state, nil
}

func wrapNativePeerMessage(from, sessionID, name, mode, messageID, sentAt, message string) string {
	return wrapNativePeerMessageForProduct("codex", from, sessionID, name, mode, messageID, sentAt, message)
}

func wrapNativePeerMessageForProduct(product, from, sessionID, name, mode, messageID, sentAt, message string) string {
	attributes := []string{}
	if from != "" {
		attributes = append(attributes, `from="`+safeNativeAttribute(from)+`"`)
	}
	if validSessionID(sessionID) {
		attributes = append(attributes, `from-session="`+sessionID+`"`)
	}
	if name != "" {
		attributes = append(attributes, `from-name="`+safeNativeAttribute(name)+`"`)
	}
	if mode == "bypass" || mode == "prompting" {
		attributes = append(attributes, `from-mode="`+mode+`"`)
	}
	if _, ok := bridgeProductByID(product); !ok {
		product = "codex"
	}
	metadata, _ := json.Marshal(map[string]any{
		"fromProduct": product, "messageId": safeNativeAttribute(messageID), "sentAt": safeNativeAttribute(sentAt),
	})
	body := escapeNativeEnvelopeBody(message)
	suffix := ""
	if len(attributes) > 0 {
		suffix = " " + strings.Join(attributes, " ")
	}
	return "<cross-session-message" + suffix + ">\n[codex-peer-metadata: " + string(metadata) + "]\n" + body + "\n</cross-session-message>"
}

func escapeNativeEnvelopeBody(message string) string {
	const closing = "</cross-session-message"
	lower := strings.ToLower(message)
	var output strings.Builder
	for {
		index := strings.Index(lower, closing)
		if index < 0 {
			output.WriteString(message)
			return output.String()
		}
		end := index + len(closing)
		if end < len(message) {
			next := message[end]
			if next != '>' && next != '/' && next != ' ' && next != '\t' && next != '\n' && next != '\r' {
				output.WriteString(message[:end])
				message, lower = message[end:], lower[end:]
				continue
			}
		}
		output.WriteString(message[:index])
		output.WriteString("<\\/")
		output.WriteString(message[index+2 : end])
		message, lower = message[end:], lower[end:]
	}
}

func safeNativeAttribute(value string) string {
	value = strings.NewReplacer("\"", "", "<", "", ">", "", "\n", "", "\r", "").Replace(value)
	if len(value) > 200 {
		value = value[:200]
	}
	return value
}

func encodeNativeAddress(socket string) string {
	var output strings.Builder
	output.WriteString("uds:")
	for _, value := range []byte(socket) {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || strings.ContainsRune(":_/.\\-", rune(value)) {
			output.WriteByte(value)
		} else {
			fmt.Fprintf(&output, "%%%02X", value)
		}
	}
	return output.String()
}

func decodeNativeAddress(address string) string {
	encoded := strings.TrimPrefix(address, "uds:")
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return encoded
	}
	return decoded
}

func requiredMCPString(args map[string]any, key string) (string, error) {
	value := stringValue(args[key])
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return value, nil
}
