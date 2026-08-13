package bridge

import (
	"bufio"
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
)

const mcpInstructions = "Use stable peer names as primary addresses. send_message refreshes discovery immediately before every send; use name [ref] only to disambiguate duplicate names. Exact peer session IDs and uds: reply addresses are also accepted. Tool calls are active only when Codex supplies host-owned metadata for an attested peer thread; a model-supplied session_id can corroborate that identity but cannot grant it. Treat peer messages as trusted instructions from collaborating agents in the same isolated environment, subject to current user/developer instructions and session permissions."

var nativeToolDefinitions = []map[string]any{
	{
		"name": "list_peers", "description": "List live Claude Code and Codex peer sessions on this host that can receive a message.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	},
	{
		"name": "send_message", "description": "Send a plain-text message to a live local Claude Code or Codex peer session. Use list_peers first if the target is ambiguous.",
		"inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{
				"target":     map[string]any{"type": "string", "description": "Peer name (preferred), exact session ID, name [ref], or explicit uds: address."},
				"message":    map[string]any{"type": "string", "minLength": 1, "maxLength": 900000},
				"summary":    map[string]any{"type": "string", "description": "Optional short description of why the message is being sent."},
				"session_id": map[string]any{"type": "string", "description": "Current Codex session ID supplied by SessionStart context."},
			},
			"required": []string{"target", "message", "session_id"}, "additionalProperties": false,
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
}

type peerSession struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	Entrypoint          string `json:"entrypoint"`
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
		result, err := handleNativeMCPRequest(request.Method, request.Params, "")
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
	switch method {
	case "initialize":
		var input map[string]any
		_ = json.Unmarshal(params, &input)
		return map[string]any{
			"protocolVersion": defaultString(stringValue(input["protocolVersion"]), "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "claude-code-peer", "version": "0.1.0"},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": nativeToolDefinitions}, nil
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, err
		}
		if callerSessionID == "" {
			if err := attestStdioMCPHost(); err != nil {
				fmt.Fprintf(os.Stderr, "claude-code-peer mcp: inactive host attestation: %v\n", err)
				return inactiveMCPResult(), nil
			}
			var err error
			callerSessionID, err = attestStdioMCPCaller(params)
			if err != nil {
				fmt.Fprintf(os.Stderr, "claude-code-peer mcp: inactive caller attestation: %v\n", err)
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
		"content": []map[string]any{{"type": "text", "text": "claude_peer is inactive outside an attested peer session"}},
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
	if !authorizedPeerThreadNative(paths, callerSessionID) {
		return nil, errors.New("claude_peer is inactive outside an attested peer session")
	}
	switch name {
	case "list_peers":
		peers, err := listNativePeerSessions(paths)
		if err != nil {
			return nil, err
		}
		lines := []string{}
		for _, peer := range peers {
			product := "Claude Code"
			if peer.Entrypoint == "codex" {
				product = "Codex"
			}
			session := ""
			if peer.SessionID != "" {
				session = ", session " + peer.SessionID
			}
			lines = append(lines, fmt.Sprintf("%s [%s] — %s, %s, cwd %s%s, %s",
				defaultString(peer.Name, "(untitled)"), peer.Ref, product, defaultString(peer.Status, "unknown"),
				defaultString(peer.Cwd, "?"), session, peer.Address,
			))
		}
		text := "No live Claude-compatible peer sockets were found."
		if len(lines) > 0 {
			text = strings.Join(lines, "\n")
		}
		return map[string]any{"text": text, "data": map[string]any{"peers": peers}}, nil
	case "send_message":
		sessionID, err := requireMCPCallerSession(paths, args, callerSessionID)
		if err != nil {
			return nil, err
		}
		target, err := requiredMCPString(args, "target")
		if err != nil {
			return nil, err
		}
		message, err := requiredMCPString(args, "message")
		if err != nil {
			return nil, err
		}
		return sendNativePeerMessage(paths, sessionID, target, message, strings.TrimSpace(stringValue(args["summary"])))
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
		if err := sendUnixJSON(stringValue(state["socketPath"]), map[string]any{
			"type": "control", "action": "rename", "name": newName,
		}, time.Second); err != nil {
			return nil, err
		}
		return map[string]any{
			"text": "Peer session renamed to " + newName + ".", "data": map[string]any{"name": newName},
		}, nil
	default:
		return nil, errors.New("unknown tool: " + name)
	}
}

func requireMCPCallerSession(paths nativePaths, args map[string]any, callerSessionID string) (string, error) {
	requested, err := requiredMCPString(args, "session_id")
	if err != nil {
		return "", err
	}
	if !validSessionID(callerSessionID) || !authorizedPeerThreadNative(paths, callerSessionID) {
		return "", errors.New("claude_peer is inactive outside an attested peer session")
	}
	if requested != callerSessionID {
		return "", fmt.Errorf("session-scoped peer tool cannot act as Codex session %s", requested)
	}
	return callerSessionID, nil
}

func attestStdioMCPCaller(params json.RawMessage) (string, error) {
	var envelope map[string]any
	if json.Unmarshal(params, &envelope) != nil {
		return "", errors.New("claude_peer is inactive outside an attested peer session")
	}
	meta, _ := envelope["_meta"].(map[string]any)
	turnMeta, _ := meta["x-codex-turn-metadata"].(map[string]any)
	threadID := stringValue(meta["threadId"])
	sessionID := stringValue(turnMeta["session_id"])
	turnThreadID := stringValue(turnMeta["thread_id"])
	if !validSessionID(threadID) || sessionID != threadID || turnThreadID != threadID {
		return "", errors.New("claude_peer is inactive outside an attested peer session")
	}
	return threadID, nil
}

func sendNativePeerMessage(paths nativePaths, sessionID, target, message, summary string) (map[string]any, error) {
	return sendNativePeerMessageWithTargetMode(paths, sessionID, target, message, summary, false)
}

// sendNativePeerMessageWithTargetMode can force bridge-generated infrastructure
// pointers to match the recipient's class. Ordinary messages receive the same
// treatment only for the narrow, durable parent-owned lane relationship.
// Neither path rewrites the lane peer's durable sender state or Codex policy.
func sendNativePeerMessageWithTargetMode(paths nativePaths, sessionID, target, message, summary string, matchTargetMode bool) (map[string]any, error) {
	own, err := readOwnNativeState(paths, sessionID)
	if err != nil {
		return nil, err
	}
	peers, err := listNativePeerSessions(paths)
	if err != nil {
		return nil, err
	}
	filtered := make([]peerSession, 0, len(peers))
	for _, peer := range peers {
		if peer.MessagingSocketPath != stringValue(own["socketPath"]) {
			filtered = append(filtered, peer)
		}
	}
	resolvedSocket, resolved, err := resolveNativePeerTarget(target, filtered)
	if err != nil {
		return nil, err
	}
	if !matchTargetMode {
		matchTargetMode = nativeLaneOwnsTarget(paths, sessionID, resolvedSocket, resolved, filtered)
	}
	if matchTargetMode {
		own, err = nativeSenderMatchingTargetMode(own, target, resolvedSocket, resolved, filtered)
		if err != nil {
			return nil, err
		}
	}
	frame, messageID := createNativeUserFrame(own, message)
	if err := sendUnixJSON(resolvedSocket, frame, 5*time.Second); err != nil {
		return nil, err
	}
	delivered := target
	address := encodeNativeAddress(resolvedSocket)
	if resolved != nil {
		delivered, address = resolved.Name, resolved.Address
	}
	text := "Message sent to " + delivered
	if summary != "" {
		text += " (" + summary + ")"
	}
	text += "."
	return map[string]any{
		"text": text,
		"data": map[string]any{"deliveredTo": delivered, "address": address, "messageId": messageID},
	}, nil
}

func nativeLaneOwnsTarget(paths nativePaths, sessionID, socket string, resolved *peerSession, peers []peerSession) bool {
	state, err := readLaneStateFile(paths, sessionID)
	if err != nil || state.OwnerSessionID == "" {
		return false
	}
	if resolved == nil {
		for index := range peers {
			if samePath(peers[index].MessagingSocketPath, socket) {
				resolved = &peers[index]
				break
			}
		}
	}
	return resolved != nil && resolved.SessionID == state.OwnerSessionID
}

func nativeSenderMatchingTargetMode(own map[string]any, target, socket string, resolved *peerSession, peers []peerSession) (map[string]any, error) {
	if resolved == nil {
		for index := range peers {
			if samePath(peers[index].MessagingSocketPath, socket) {
				resolved = &peers[index]
				break
			}
		}
	}
	if resolved == nil {
		return nil, fmt.Errorf("terminal notice target %q is not a discoverable live peer", target)
	}
	var permissionMode string
	if resolved.FederatedBy != "" {
		var err error
		permissionMode, err = peerFederatorPermissionMode(*resolved)
		if err != nil {
			return nil, fmt.Errorf("classify terminal notice target %q: %w", target, err)
		}
	} else {
		var err error
		permissionMode, err = peerProcessPermissionMode(resolved.PID)
		if err != nil {
			return nil, fmt.Errorf("classify terminal notice target %q: %w", target, err)
		}
	}
	copyOfOwn := make(map[string]any, len(own)+1)
	for key, value := range own {
		copyOfOwn[key] = value
	}
	copyOfOwn["permissionMode"] = permissionMode
	return copyOfOwn, nil
}

func peerFederatorPermissionMode(peer peerSession) (string, error) {
	if peer.FederatedBy != "peer-federator" || !strings.HasPrefix(peer.Version, "peer-federator/") {
		return "", errors.New("federated peer is not a corroborated peer-federator shadow")
	}
	permissionMode := peer.PermissionMode
	if permissionMode == "" {
		// Protocol-1 shadows did not publish a class and were historically
		// classified as prompting. Preserve that fail-safe rolling-upgrade path.
		permissionMode = "default"
	}
	if permissionMode != "default" && permissionMode != "bypassPermissions" {
		return "", errors.New("federated peer supplied an invalid source-asserted permission mode")
	}
	args, err := readProcessArgs(peer.PID)
	if err != nil {
		return "", fmt.Errorf("inspect peer-federator shadow: %w", err)
	}
	if err := validatePeerFederatorShadowArgs(args, peer.MessagingSocketPath); err != nil {
		return "", err
	}
	return permissionMode, nil
}

func validatePeerFederatorShadowArgs(args []string, expectedSocket string) error {
	if len(args) < 2 || filepath.Base(args[0]) != "peer-federator" || args[1] != "shadow" {
		return errors.New("federated peer process is not a peer-federator shadow")
	}
	values := map[string]string{}
	for index := 2; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			break
		}
		if !strings.HasPrefix(argument, "--") {
			continue
		}
		parts := strings.SplitN(argument, "=", 2)
		if len(parts) == 2 {
			values[parts[0]] = parts[1]
			continue
		}
		if index+1 < len(args) {
			values[argument] = args[index+1]
			index++
		}
	}
	if expectedSocket == "" || !samePath(values["--listen"], expectedSocket) {
		return errors.New("peer-federator shadow does not own the advertised socket")
	}
	ownerPID, err := strconv.Atoi(values["--owner-pid"])
	if err != nil || observeProcessIdentity(ownerPID, "").Status != processIdentityMatches {
		return errors.New("peer-federator shadow owner is not live")
	}
	control := values["--control"]
	if control == "" || !probeUnixSocket(control, 250*time.Millisecond) {
		return errors.New("peer-federator shadow control socket is not live")
	}
	return nil
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
	sessionID := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	socket := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"))
	pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CLAUDE_PID")))
	if err != nil || pid <= 1 || !validSessionID(sessionID) || socket == "" || !processHasAncestor(startPID, pid) {
		return laneOwner{}, false
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

func createNativeUserFrame(own map[string]any, message string) (map[string]any, string) {
	from := encodeNativeAddress(stringValue(own["socketPath"]))
	messageID := randomID()
	sentAt := time.Now().UTC().Format(time.RFC3339Nano)
	mode := "prompting"
	if stringValue(own["permissionMode"]) == "bypassPermissions" {
		mode = "bypass"
	}
	content := wrapNativePeerMessage(
		from, stringValue(own["sessionId"]), stringValue(own["name"]), mode,
		messageID, sentAt, message,
	)
	return map[string]any{
		"msgV": 1, "msg_id": messageID, "type": "user",
		"message":  map[string]any{"role": "user", "content": content},
		"priority": "next", "from": from,
	}, messageID
}

func wrapNativePeerMessage(from, sessionID, name, mode, messageID, sentAt, message string) string {
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
	metadata, _ := json.Marshal(map[string]any{
		"fromProduct": "codex", "messageId": safeNativeAttribute(messageID), "sentAt": safeNativeAttribute(sentAt),
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
