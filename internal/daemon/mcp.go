package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const mcpServerInstructions = "Use Agent Sessions tools for discovery and delivery within this managed peer's existing global groups. Installation alone grants no authority: tool calls are inactive unless this connector is attached to an exactly attested native session."

// MCPForwardRequest is one vendor-framed MCP request relayed without local decisions.
type MCPForwardRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// MCPProtocolError is one JSON-RPC error the stateless relay must preserve.
type MCPProtocolError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPForwardResult is the daemon-owned MCP decision returned to a stateless relay.
type MCPForwardResult struct {
	Result json.RawMessage   `json:"result,omitempty"`
	Error  *MCPProtocolError `json:"error,omitempty"`
}

// MCPService owns the model-facing tool inventory, authorization decisions,
// and routing. Vendor-required child processes only translate stdio framing.
type MCPService struct {
	attachments *AttachmentRegistry
	deliveries  *DeliveryEngine
	lanes       *LaneEngine
	messageID   func() (string, error)
	laneCommand func(context.Context, controlPrincipal, LaneCommandRequest) (map[string]any, error)
}

func newMCPService(attachments *AttachmentRegistry, deliveries *DeliveryEngine, lanes *LaneEngine) (*MCPService, error) {
	if attachments == nil || deliveries == nil || lanes == nil {
		return nil, errors.New("MCP service requires attachment, delivery, and lane authorities")
	}
	service := &MCPService{attachments: attachments, deliveries: deliveries, lanes: lanes, messageID: randomControlRequestID}
	service.laneCommand = func(ctx context.Context, principal controlPrincipal, request LaneCommandRequest) (map[string]any, error) {
		return executeLaneCommand(ctx, lanes, attachments, principal.AttachmentID, request)
	}
	return service, nil
}

// Forward handles one closed MCP method inventory without trusting model-supplied identity.
func (service *MCPService) Forward(ctx context.Context, principal controlPrincipal, request MCPForwardRequest) MCPForwardResult {
	switch request.Method {
	case "initialize":
		var input map[string]any
		_ = json.Unmarshal(request.Params, &input)
		return mcpResult(map[string]any{
			"protocolVersion": defaultMCPString(input["protocolVersion"], "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-sessions", "version": BuildVersion},
			"instructions":    mcpServerInstructions,
		})
	case "ping":
		return mcpResult(map[string]any{})
	case "tools/list":
		return mcpResult(map[string]any{"tools": daemonMCPToolDefinitions()})
	case "tools/call":
		return service.call(ctx, principal, request.Params)
	default:
		return MCPForwardResult{Error: &MCPProtocolError{Code: -32601, Message: "Method not found: " + request.Method}}
	}
}

//nolint:gocyclo // Tool routing shares one attested-session authorization and protocol-error boundary.
func (service *MCPService) call(ctx context.Context, principal controlPrincipal, params json.RawMessage) MCPForwardResult {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || strings.TrimSpace(call.Name) == "" {
		return MCPForwardResult{Error: &MCPProtocolError{Code: -32602, Message: "Invalid tools/call parameters"}}
	}
	if !principal.Attested || principal.AttachmentID == "" || principal.SessionID == "" {
		return mcpToolFailure("agent_sessions is inactive outside an attested peer session")
	}
	if requested := strings.TrimSpace(mcpString(call.Arguments["session_id"])); requested != "" && requested != principal.SessionID {
		return mcpToolFailure("session-scoped peer tool cannot act as another session")
	}
	var (
		result any
		err    error
	)
	switch call.Name {
	case "identity":
		var record AttachmentRecord
		record, err = service.attachments.Select(ctx, AttachmentSelector{SessionID: principal.SessionID})
		if err == nil {
			address := attachmentAddress(record)
			result = map[string]any{"name": record.Name, "address": address, "session_id": record.SessionID, "product": record.Product}
		}
	case "list_peers":
		result, err = service.discover(ctx, principal)
	case "send_message":
		result, err = service.send(ctx, principal, call.Arguments)
	case "broadcast":
		result, err = service.broadcast(ctx, principal, call.Arguments)
	case "lane":
		result, err = service.lane(ctx, principal, call.Arguments)
	case "check_inbox", "rename_session":
		err = fmt.Errorf("%s is not available in this daemon generation", call.Name)
	default:
		err = fmt.Errorf("unknown tool: %s", call.Name)
	}
	if err != nil {
		return mcpToolFailure(err.Error())
	}
	return mcpToolSuccess(result)
}

func (service *MCPService) lane(ctx context.Context, principal controlPrincipal, arguments map[string]any) (map[string]any, error) {
	product, err := requiredMCPArgument(arguments, "product")
	if err != nil {
		return nil, err
	}
	command, err := requiredMCPArgument(arguments, "command")
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(mcpString(arguments["host"]))
	nativeArguments, err := mcpLaneCommandArguments(arguments["arguments"])
	if err != nil {
		return nil, err
	}
	return service.laneCommand(ctx, principal, LaneCommandRequest{
		Product: product, Command: command, Host: host, Arguments: nativeArguments, Input: mcpString(arguments["input"]),
	})
}

func mcpLaneCommandArguments(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok || len(values) > 256 {
		return nil, errors.New("lane arguments must be an array of at most 256 strings")
	}
	result := make([]string, 0, len(values))
	total := 0
	for _, value := range values {
		text, ok := value.(string)
		if !ok || strings.ContainsRune(text, '\x00') || len(text) > 4096 {
			return nil, errors.New("lane arguments must contain bounded valid strings")
		}
		total += len(text)
		if total > 512*1024 {
			return nil, errors.New("lane arguments exceed the size limit")
		}
		result = append(result, text)
	}
	return result, nil
}

func (service *MCPService) discover(ctx context.Context, principal controlPrincipal) (map[string]any, error) {
	peers, err := service.deliveries.Discover(ctx, principal.AttachmentID)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(peers))
	for _, peer := range peers {
		lines = append(lines, fmt.Sprintf("%s — %s, %s, session %s, groups %s",
			defaultMCPString(peer.DisplayName, peer.Name), peer.Entrypoint,
			defaultMCPString(peer.Status, "unknown"), peer.SessionID, strings.Join(peer.Groups, ",")))
	}
	text := "No live peers share a group with this session."
	if len(lines) != 0 {
		text = strings.Join(lines, "\n")
	}
	return map[string]any{"text": text, "peers": peers}, nil
}

func (service *MCPService) send(ctx context.Context, principal controlPrincipal, arguments map[string]any) (map[string]any, error) {
	message, err := requiredMCPArgument(arguments, "message")
	if err != nil {
		return nil, err
	}
	targets, err := mcpTargets(arguments)
	if err != nil {
		return nil, err
	}
	messageID, err := service.messageID()
	if err != nil {
		return nil, err
	}
	operation := DeliveryOperationSend
	if len(targets) > 1 {
		operation = DeliveryOperationMulticast
	}
	record, err := service.deliveries.Accept(ctx, DeliveryRequest{
		MessageID: messageID, SourceAttachmentID: principal.AttachmentID, Operation: operation,
		Targets: targets, Content: message, Summary: strings.TrimSpace(mcpString(arguments["summary"])),
	})
	if err != nil {
		return nil, err
	}
	return mcpDeliveryProjection(record), nil
}

func (service *MCPService) broadcast(ctx context.Context, principal controlPrincipal, arguments map[string]any) (map[string]any, error) {
	group, err := requiredMCPArgument(arguments, "group")
	if err != nil {
		return nil, err
	}
	message, err := requiredMCPArgument(arguments, "message")
	if err != nil {
		return nil, err
	}
	messageID, err := service.messageID()
	if err != nil {
		return nil, err
	}
	record, err := service.deliveries.Accept(ctx, DeliveryRequest{
		MessageID: messageID, SourceAttachmentID: principal.AttachmentID, Operation: DeliveryOperationBroadcast,
		Group: group, Content: message, Summary: strings.TrimSpace(mcpString(arguments["summary"])),
	})
	if err != nil {
		return nil, err
	}
	return mcpDeliveryProjection(record), nil
}

func mcpDeliveryProjection(record DeliveryRecord) map[string]any {
	return map[string]any{
		"text":       fmt.Sprintf("Agent Sessions %s %s is %s for %d peer(s).", record.Operation, record.MessageID, record.State, len(record.ResolvedDestinations)),
		"message_id": record.MessageID, "operation": record.Operation, "state": record.State,
		"destinations": record.ResolvedDestinations, "destination_results": record.DestinationResults,
	}
}

func mcpResult(value any) MCPForwardResult {
	body, err := json.Marshal(value)
	if err != nil {
		return MCPForwardResult{Error: &MCPProtocolError{Code: -32603, Message: "encode MCP result"}}
	}
	return MCPForwardResult{Result: body}
}

func mcpToolSuccess(value any) MCPForwardResult {
	text := ""
	if object, ok := value.(map[string]any); ok {
		text = mcpString(object["text"])
	}
	if text == "" {
		body, _ := json.Marshal(value)
		text = string(body)
	}
	return mcpResult(map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": value, "isError": false,
	})
}

func mcpToolFailure(message string) MCPForwardResult {
	return mcpResult(map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}}, "isError": true,
	})
}

func requiredMCPArgument(arguments map[string]any, name string) (string, error) {
	value := strings.TrimSpace(mcpString(arguments[name]))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func mcpTargets(arguments map[string]any) ([]string, error) {
	single := strings.TrimSpace(mcpString(arguments["target"]))
	targets := make([]string, 0)
	switch values := arguments["targets"].(type) {
	case nil:
	case []any:
		for _, value := range values {
			target := strings.TrimSpace(mcpString(value))
			if target == "" {
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
		targets = append(targets, single)
	}
	if len(targets) == 0 {
		return nil, errors.New("send_message requires target or targets")
	}
	sort.Strings(targets)
	for index := 1; index < len(targets); index++ {
		if targets[index] == targets[index-1] {
			return nil, errors.New("delivery targets must be unique")
		}
	}
	return targets, nil
}

func mcpString(value any) string {
	text, _ := value.(string)
	return text
}

func defaultMCPString(value any, fallback string) string {
	if text := strings.TrimSpace(mcpString(value)); text != "" {
		return text
	}
	return fallback
}

func daemonMCPToolDefinitions() []map[string]any {
	return []map[string]any{
		mcpTool("list_peers", "List live peers sharing at least one existing global group with this managed session", map[string]any{}, nil),
		mcpTool("send_message", "Send one direct message or explicit multicast within existing shared groups", map[string]any{
			"target": map[string]any{"type": "string"}, "targets": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1},
			"message": map[string]any{"type": "string", "minLength": 1}, "summary": map[string]any{"type": "string"},
		}, []string{"message"}),
		mcpTool("broadcast", "Broadcast to every other current member of one existing global group", map[string]any{
			"group": map[string]any{"type": "string", "minLength": 1}, "message": map[string]any{"type": "string", "minLength": 1}, "summary": map[string]any{"type": "string"},
		}, []string{"group", "message"}),
		mcpTool("check_inbox", "Read the bounded pending message projection for this managed session", map[string]any{}, nil),
		mcpTool("identity", "Show this managed session's exact Agent Sessions identity", map[string]any{}, nil),
		mcpTool("rename_session", "Update this managed session's display name", map[string]any{"name": map[string]any{"type": "string", "minLength": 1, "maxLength": 80}}, []string{"name"}),
		mcpTool("lane", "Run one supported durable lane lifecycle operation for this attested parent", map[string]any{
			"product":   map[string]any{"type": "string", "enum": []string{"codex", "claude", "grok", "qwen"}},
			"command":   map[string]any{"type": "string", "enum": []string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"}},
			"arguments": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"input":     map[string]any{"type": "string", "maxLength": maxLaneCommandInputBytes},
			"host":      map[string]any{"type": "string", "description": "Reserved for connected-host routing"},
		}, []string{"product", "command"}),
	}
}

func mcpTool(name, description string, properties map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		schema["required"] = required
	}
	return map[string]any{
		"name": name, "description": description,
		"inputSchema": schema,
	}
}
