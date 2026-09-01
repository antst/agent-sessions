package sessiontools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const maxMCPInputBytes = 1 << 20

var instructions = map[string]string{
	"codex":  "Use stable peer names as primary addresses. Discovery and delivery are limited to peers sharing this session's Agent Sessions groups. send_message supports one target or an explicit multicast; broadcast requires a group this session belongs to. lane runs an exact Codex, Claude, Grok, or Qwen lane lifecycle command outside the caller's shell sandbox while retaining the attested parent identity; use it instead of a sandboxed lane executable. Tool calls activate only when Codex supplies host-owned metadata matching an attested grouped peer thread; session_id is optional corroboration.",
	"claude": "Use these structured tools for every Agent Sessions discovery, send, multicast, broadcast, acknowledgment, reply, and Codex, Claude, Grok, or Qwen lane lifecycle operation from this managed Claude session. Use lane instead of invoking a product lane executable through Bash; the Agent Sessions daemon owns the background worker and lifecycle. For an incoming delivery, send_message back to source.id (or source.name when unique). Never send plain text to the native agent-sessions--HOST service: native SendMessage reports only carrier acceptance and does not route unframed text. This MCP process activates only for exact ancestry from the live native Claude adapter plus its matching grouped host-agent registration; no model-supplied session_id is required.",
	"grok":   "Use stable peer names as primary addresses. Discovery and delivery are limited to peers sharing this session's Agent Sessions groups. send_message supports one target or an explicit multicast; broadcast requires a group this session belongs to. lane runs an exact Codex, Claude, Grok, or Qwen lane lifecycle command outside the caller's shell sandbox while retaining the attested parent identity; use it instead of a shell-executed lane launcher. This MCP process activates only for a live process-attested grok-peer launch with a matching grouped host-agent registration; session_id is optional corroboration.",
	"qwen":   "Use these structured Agent Sessions tools for discovery, direct sends, multicast, broadcast, inbox recovery, identity, rename, and foreign lane lifecycle commands from this managed Qwen session. This MCP process activates only for exact process ancestry from the live qwen-peer adapter with a matching grouped host-agent registration; session_id is optional corroboration. Qwen's native approval mode remains Qwen-owned and may change during the session.",
}

type mcpToolPolicy struct {
	contextName        string
	sessionDescription string
	allowedTools       map[string]bool
	requiresSession    bool
}

var mcpToolPolicies = map[string]mcpToolPolicy{
	"codex": {requiresSession: true},
	"claude": {
		contextName:        "managed Claude",
		sessionDescription: "Optional corroboration of this Claude session UUID; process ancestry is authoritative.",
		allowedTools:       map[string]bool{"list_peers": true, "send_message": true, "broadcast": true, "rename_session": true, "lane": true},
	},
	"grok": {
		contextName:        "Grok",
		sessionDescription: "Optional corroboration of the current Grok session ID. The launch token is authoritative.",
	},
	"qwen": {
		contextName:        "Qwen",
		sessionDescription: "Optional corroboration of the current Qwen session ID. Exact process and host registration attestation are authoritative.",
	},
}

func ProductMCPInstructions(product string) (string, error) {
	if _, ok := productcatalog.ByID(product); !ok {
		return "", fmt.Errorf("MCP instruction product %q is unsupported", product)
	}
	instruction, ok := instructions[product]
	if !ok {
		return "", fmt.Errorf("MCP instructions for product %q are unavailable", product)
	}
	return instruction, nil
}

func ProductMCPTools(product string) ([]map[string]any, error) {
	_, ok := productcatalog.ByID(product)
	if !ok {
		return nil, fmt.Errorf("MCP tool product %q is unsupported", product)
	}
	policy, ok := mcpToolPolicies[product]
	if !ok {
		return nil, fmt.Errorf("MCP tool policy for product %q is unavailable", product)
	}
	definitions := baseToolDefinitions()
	// The original Codex surface was authored as []string requirements. Each
	// call already receives a fresh definition tree, so return it directly and
	// preserve that public Go shape. The non-Codex projections historically
	// passed through JSON before removing model-supplied session authority; keep
	// that []any shape for exact original-four compatibility.
	if policy.requiresSession {
		return definitions, nil
	}
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
		if session, ok := properties["session_id"].(map[string]any); ok {
			session["description"] = policy.sessionDescription
		}
		filtered = append(filtered, definition)
	}
	return filtered, nil
}

func baseToolDefinitions() []map[string]any {
	laneProducts := make([]string, 0)
	for _, descriptor := range productcatalog.All() {
		if descriptor.Has(productcatalog.CapabilityLane) {
			laneProducts = append(laneProducts, descriptor.ID)
		}
	}
	return []map[string]any{
		{"name": "list_peers", "description": "List live local or federated Agent Sessions peers that share at least one group with this session.", "inputSchema": objectSchema(map[string]any{}, nil)},
		{"name": "send_message", "description": "Send a plain-text message to one peer or an explicit multicast set visible through this session's groups. Use list_peers first if a target is ambiguous.", "inputSchema": objectSchema(map[string]any{
			"target": map[string]any{"type": "string", "description": "Visible peer name, exact session ID, display name, or host/session ID."}, "targets": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Explicit multicast recipients. Use either target or targets."},
			"message": map[string]any{"type": "string", "minLength": 1, "maxLength": 900000}, "summary": map[string]any{"type": "string", "description": "Optional short description of why the message is being sent."}, "session_id": sessionProperty("Current Codex session ID supplied by SessionStart context."),
		}, []string{"message", "session_id"})},
		{"name": "broadcast", "description": "Broadcast a plain-text message to every other current member of one group this session belongs to.", "inputSchema": objectSchema(map[string]any{
			"group": map[string]any{"type": "string", "minLength": 1}, "message": map[string]any{"type": "string", "minLength": 1, "maxLength": 900000}, "summary": map[string]any{"type": "string"}, "session_id": sessionProperty(""),
		}, []string{"group", "message", "session_id"})},
		{"name": "check_inbox", "description": "Recovery-only: read and consume peer messages queued past an automatic delivery boundary. Active peer messages are pushed into the session automatically; do not poll this tool.", "inputSchema": objectSchema(map[string]any{"session_id": sessionProperty("Current Codex session ID supplied by SessionStart context.")}, []string{"session_id"})},
		{"name": "identity", "description": "Show this Codex session's Claude-compatible peer name and address.", "inputSchema": objectSchema(map[string]any{"session_id": sessionProperty("Current Codex session ID supplied by SessionStart context.")}, []string{"session_id"})},
		{"name": "rename_session", "description": "Change this managed session's public Agent Sessions peer name.", "inputSchema": objectSchema(map[string]any{"session_id": sessionProperty("Current Codex session ID supplied by SessionStart context."), "name": map[string]any{"type": "string", "minLength": 1, "maxLength": 80}}, []string{"session_id", "name"})},
		{"name": "lane", "description": "Run one exact local or federated supported-product lane lifecycle command for this attested parent. Use this instead of invoking a lane executable from a sandboxed shell.", "inputSchema": objectSchema(map[string]any{
			"product": map[string]any{"type": "string", "enum": laneProducts}, "command": map[string]any{"type": "string", "enum": []string{"doctor", "list", "run", "start", "resume", "wait", "status", "interrupt", "archive"}},
			"arguments": map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": 4096}, "maxItems": 256, "description": "Native arguments after the lifecycle command."}, "input": map[string]any{"type": "string", "maxLength": maxMCPInputBytes, "description": "Optional stdin briefing for run, start, or resume."}, "host": map[string]any{"type": "string", "description": "Optional connected destination host id or unique name. Omit for a local lane."}, "session_id": sessionProperty("Current Codex session ID supplied by SessionStart context."),
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
