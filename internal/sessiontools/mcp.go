package sessiontools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const maxMCPInputBytes, maxMCPArguments, maxMCPArgumentLength = 1 << 20, 256, 4096

var laneCommands = []string{"doctor", "list", "run", "start", "resume", "steer", "wait", "status", "interrupt", "archive"}

func ValidateMCPToolCall(product string, raw json.RawMessage) map[string]any {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &call) != nil || call.Name != "lane" {
		return nil
	}
	invalid := func(method string) map[string]any {
		if method == "" {
			return map[string]any{"tool": "lane"}
		}
		return map[string]any{"method": "lane." + method}
	}
	var fields map[string]json.RawMessage
	if len(call.Arguments) == 0 || string(call.Arguments) == "null" || json.Unmarshal(call.Arguments, &fields) != nil {
		return invalid("")
	}
	var command string
	if value, ok := fields["command"]; !ok || json.Unmarshal(value, &command) != nil {
		return invalid("")
	}
	if !slices.Contains(laneCommands, command) {
		return invalid(command)
	}
	allowed := map[string]bool{"product": true, "command": true, "arguments": true, "input": true, "host": true}
	if !mcpToolPolicies[product].omitSession {
		allowed["session_id"] = true
	}
	for key := range fields {
		if !allowed[key] {
			return invalid(command)
		}
	}
	var token string
	if value, ok := fields["product"]; !ok || json.Unmarshal(value, &token) != nil || productcatalog.ValidateToken(token) != nil {
		return invalid(command)
	}
	if value, ok := fields["arguments"]; ok {
		var arguments []string
		if string(value) == "null" || json.Unmarshal(value, &arguments) != nil || len(arguments) > maxMCPArguments {
			return invalid(command)
		}
		for _, argument := range arguments {
			if utf8.RuneCountInString(argument) > maxMCPArgumentLength {
				return invalid(command)
			}
		}
	}
	for _, field := range []string{"host", "session_id"} {
		if value, ok := fields[field]; ok && json.Unmarshal(value, new(string)) != nil {
			return invalid(command)
		}
	}
	if value, ok := fields["input"]; ok {
		var input string
		if json.Unmarshal(value, &input) != nil || utf8.RuneCountInString(input) > maxMCPInputBytes {
			return invalid(command)
		}
	}
	return nil
}

const BugReportGuidance = "If Agent Sessions behaves contrary to this description or its documentation and the gh CLI is authorized in your environment, you are encouraged to open an issue on github.com/antst/agent-sessions with gh issue create, including the exact command, observed behavior, and expected behavior."

const genericInstructions = "Use stable peer names with these structured tools for Agent Sessions peer discovery, live messaging, and token-named product lane lifecycle operations from this managed product session. The daemon decides whether a lane product is launchable; the current live product session supplies the caller identity and session_id is optional context. " + BugReportGuidance

var instructions = map[string]string{
	"claude": "Use these structured tools for Agent Sessions discovery, direct or multicast messaging, replies, and supported-product lane lifecycle operations from this live Claude session. Use lane instead of invoking a product lane executable through Bash. For an incoming delivery, send_message back to the source UUID or unique name.",
	"qwen":   "Use these structured Agent Sessions tools for discovery, direct sends, multicast, group sends, identity, rename, and foreign lane lifecycle commands from this live Qwen session. Qwen's native approval mode remains Qwen-owned and may change during the session.",
}

type mcpToolPolicy struct {
	contextName        string
	sessionDescription string
	allowedTools       map[string]bool
	omitSession        bool
}

var mcpToolPolicies = map[string]mcpToolPolicy{
	"codex": {contextName: "Codex", omitSession: true},
	"claude": {
		contextName:        "managed Claude",
		sessionDescription: "Optional context for this live Claude session UUID.",
		allowedTools:       map[string]bool{"list_peers": true, "send_message": true, "lane": true},
	},
	"grok": {
		contextName:        "Grok",
		sessionDescription: "Optional context for the current live Grok session ID.",
	},
	"qwen": {
		contextName:        "Qwen",
		sessionDescription: "Optional context for the current live Qwen session ID.",
	},
}

func ProductMCPInstructions(product string) (string, error) {
	if _, ok := productcatalog.ByID(product); !ok {
		return "", fmt.Errorf("MCP instruction product %q is unsupported", product)
	}
	instruction := instructions[product]
	if instruction == "" {
		instruction = genericInstructions
	}
	return instruction, nil
}

func ProductMCPTools(product string) ([]map[string]any, error) {
	_, ok := productcatalog.ByID(product)
	if !ok {
		return nil, fmt.Errorf("MCP tool product %q is unsupported", product)
	}
	policy := mcpToolPolicies[product]
	definitions := baseToolDefinitions()
	body, err := json.Marshal(definitions)
	if err != nil {
		return nil, err
	}
	var clone []map[string]any
	if err := json.Unmarshal(body, &clone); err != nil {
		return nil, err
	}
	filtered := clone[:0]
	for _, definition := range clone {
		name, _ := definition["name"].(string)
		if policy.allowedTools != nil && !policy.allowedTools[name] {
			continue
		}
		description, _ := definition["description"].(string)
		definition["description"] = strings.ReplaceAll(description, "this Codex session", "this "+policy.contextName+" session")
		schema, _ := definition["inputSchema"].(map[string]any)
		removeRequired(schema, "session_id")
		properties, _ := schema["properties"].(map[string]any)
		if policy.omitSession {
			delete(properties, "session_id")
		} else if session, ok := properties["session_id"].(map[string]any); ok {
			session["description"] = policy.sessionDescription
		}
		filtered = append(filtered, definition)
	}
	return filtered, nil
}

func baseToolDefinitions() []map[string]any {
	sendSchema := objectSchema(map[string]any{
		"target":  map[string]any{"type": "string", "minLength": 1, "description": "Visible peer name, exact session ID, display name, or host/session ID."},
		"targets": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "uniqueItems": true, "description": "Explicit multicast recipients."},
		"group":   map[string]any{"type": "string", "minLength": 1, "description": "One group this session belongs to; sends to every other current member."},
		"message": map[string]any{"type": "string", "minLength": 1, "maxLength": 900000}, "session_id": sessionProperty("Current Codex session ID supplied by SessionStart context."),
	}, []string{"message", "session_id"})
	sendSchema["oneOf"] = []map[string]any{
		{"required": []string{"target"}},
		{"required": []string{"targets"}},
		{"required": []string{"group"}},
	}
	return []map[string]any{
		{"name": "list_peers", "description": "List live local or federated Agent Sessions peers that share at least one group with this session.", "inputSchema": objectSchema(map[string]any{}, nil)},
		{"name": "send_message", "description": "Send plain text to one peer, an explicit multicast set, or every other current member of one group this session belongs to. Use list_peers first if a target is ambiguous.", "inputSchema": sendSchema},
		{"name": "check_inbox", "description": "Recovery-only: read and consume peer messages queued past an automatic delivery boundary. Active peer messages are pushed into the session automatically; do not poll this tool.", "inputSchema": objectSchema(map[string]any{"session_id": sessionProperty("Current Codex session ID supplied by SessionStart context.")}, []string{"session_id"})},
		{"name": "identity", "description": "Show this Codex session's Claude-compatible peer name and address.", "inputSchema": objectSchema(map[string]any{"session_id": sessionProperty("Current Codex session ID supplied by SessionStart context.")}, []string{"session_id"})},
		{"name": "rename_session", "description": "Change this managed session's public Agent Sessions peer name.", "inputSchema": objectSchema(map[string]any{"session_id": sessionProperty("Current Codex session ID supplied by SessionStart context."), "name": map[string]any{"type": "string", "minLength": 1, "maxLength": 80}}, []string{"session_id", "name"})},
		{"name": "lane", "description": "Run one exact local or federated token-named product lane lifecycle command for this attested parent. The daemon alone decides whether the product is launchable.", "inputSchema": objectSchema(map[string]any{
			"product": map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": `^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`, "description": "Opaque product token resolved by the daemon."}, "command": map[string]any{"type": "string", "enum": append([]string(nil), laneCommands...)},
			"arguments": map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": maxMCPArgumentLength}, "maxItems": maxMCPArguments, "description": "Native arguments after the lifecycle command."}, "input": map[string]any{"type": "string", "maxLength": maxMCPInputBytes, "description": "Optional stdin briefing for run, start, or resume."}, "host": map[string]any{"type": "string", "description": "Optional connected destination host id or unique name. Omit for a local lane."}, "session_id": sessionProperty("Current Codex session ID supplied by SessionStart context."),
		}, []string{"product", "command", "session_id"})},
	}
}

func sessionProperty(description string) map[string]any {
	property := map[string]any{"type": "string"}
	if description != "" {
		property["description"] = description
	}
	return property
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if required != nil {
		result["required"] = required
	}
	return result
}

func removeRequired(schema map[string]any, name string) {
	switch required := schema["required"].(type) {
	case []any:
		kept := required[:0]
		for _, item := range required {
			if item != name {
				kept = append(kept, item)
			}
		}
		schema["required"] = kept
	case []string:
		kept := required[:0]
		for _, item := range required {
			if item != name {
				kept = append(kept, item)
			}
		}
		schema["required"] = kept
	}
}
