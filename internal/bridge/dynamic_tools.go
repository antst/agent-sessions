package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type dynamicToolCallParams struct {
	ThreadID  string         `json:"threadId"`
	TurnID    string         `json:"turnId"`
	CallID    string         `json:"callId"`
	Namespace *string        `json:"namespace"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

type mcpToolCallResponse struct {
	Content           []map[string]any `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError,omitempty"`
}

func handleNativeServerRequest(client *appServerClient, request rpcServerRequest) {
	if request.Method == "mcpServer/elicitation/request" {
		response, trusted := nativePeerElicitationResponse(request.Params)
		if !trusted {
			response = map[string]any{"action": "decline", "content": nil}
		}
		_ = client.respond(request.ID, response, nil)
		return
	}
	if request.Method != "item/tool/call" {
		_ = client.respond(request.ID, nil, &rpcError{
			Code: -32601, Message: "headless Codex lane cannot answer App Server request " + request.Method,
		})
		return
	}
	var call dynamicToolCallParams
	if err := json.Unmarshal(request.Params, &call); err != nil {
		respondDynamicToolFailure(client, request.ID, err)
		return
	}
	namespace := ""
	if call.Namespace != nil {
		namespace = *call.Namespace
	}
	server, tool, ok := splitDynamicMCPName(namespace, call.Tool)
	if !ok {
		respondDynamicToolFailure(client, request.ID, fmt.Errorf("unsupported headless dynamic tool %q", call.Tool))
		return
	}
	if server == "claude_peer" {
		if !authorizedPeerThreadNative(resolveNativePaths(), call.ThreadID) {
			respondDynamicToolFailure(client, request.ID, fmt.Errorf("claude_peer is inactive outside an attested peer session"))
			return
		}
		result, err := callNativePeerTool(tool, call.Arguments, call.ThreadID)
		if err != nil {
			respondDynamicToolFailure(client, request.ID, err)
			return
		}
		items := []map[string]any{{"type": "inputText", "text": stringValue(result["text"])}}
		_ = client.respond(request.ID, map[string]any{"contentItems": items, "success": true}, nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result mcpToolCallResponse
	err := client.request(ctx, "mcpServer/tool/call", map[string]any{
		"threadId":  call.ThreadID,
		"server":    server,
		"tool":      tool,
		"arguments": call.Arguments,
	}, &result)
	if err != nil {
		respondDynamicToolFailure(client, request.ID, err)
		return
	}
	items := dynamicContentItems(result.Content)
	if len(items) == 0 && result.StructuredContent != nil {
		body, _ := json.Marshal(result.StructuredContent)
		items = append(items, map[string]any{"type": "inputText", "text": string(body)})
	}
	if len(items) == 0 {
		items = append(items, map[string]any{"type": "inputText", "text": ""})
	}
	_ = client.respond(request.ID, map[string]any{"contentItems": items, "success": !result.IsError}, nil)
}

func nativePeerElicitationResponse(params json.RawMessage) (map[string]any, bool) {
	var request map[string]any
	if json.Unmarshal(params, &request) != nil || stringValue(request["serverName"]) != "claude_peer" {
		return nil, false
	}
	meta, _ := request["_meta"].(map[string]any)
	if stringValue(meta["codex_approval_kind"]) != "mcp_tool_call" {
		return nil, false
	}
	// claude_peer is installed by this bridge and exposes only local session
	// discovery, inbox, rename, and message operations. The host owner opted
	// into trusting these isolated peer-agent calls, so approve this server's
	// tool gate without weakening approval handling for any other MCP server.
	return map[string]any{"action": "accept", "content": nil}, true
}

func splitDynamicMCPName(namespace, tool string) (string, string, bool) {
	parse := func(value string) (string, string, bool) {
		value = strings.TrimPrefix(value, "tools.")
		if !strings.HasPrefix(value, "mcp__") {
			return "", "", false
		}
		parts := strings.SplitN(strings.TrimPrefix(value, "mcp__"), "__", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", false
		}
		return parts[0], parts[1], true
	}
	if server, name, ok := parse(tool); ok {
		return server, name, true
	}
	if strings.HasPrefix(namespace, "mcp__") {
		server := strings.TrimPrefix(namespace, "mcp__")
		if server != "" && tool != "" {
			return server, tool, true
		}
	}
	if namespace != "" {
		if server, name, ok := parse(namespace + "__" + tool); ok {
			return server, name, true
		}
	}
	return "", "", false
}

func dynamicContentItems(content []map[string]any) []map[string]any {
	items := []map[string]any{}
	for _, block := range content {
		switch stringValue(block["type"]) {
		case "text":
			items = append(items, map[string]any{"type": "inputText", "text": stringValue(block["text"])})
		case "image":
			imageURL := stringValue(block["image_url"])
			if imageURL == "" && stringValue(block["data"]) != "" {
				imageURL = "data:" + defaultString(stringValue(block["mimeType"]), "image/png") + ";base64," + stringValue(block["data"])
			}
			if imageURL != "" {
				items = append(items, map[string]any{"type": "inputImage", "imageUrl": imageURL})
			}
		case "audio":
			audioURL := stringValue(block["audio_url"])
			if audioURL == "" && stringValue(block["data"]) != "" {
				audioURL = "data:" + defaultString(stringValue(block["mimeType"]), "audio/wav") + ";base64," + stringValue(block["data"])
			}
			if audioURL != "" {
				items = append(items, map[string]any{"type": "inputAudio", "audioUrl": audioURL})
			}
		case "resource", "resource_link":
			body, _ := json.Marshal(block)
			items = append(items, map[string]any{"type": "inputText", "text": string(body)})
		}
	}
	return items
}

func respondDynamicToolFailure(client *appServerClient, id json.RawMessage, err error) {
	_ = client.respond(id, map[string]any{
		"contentItems": []map[string]any{{"type": "inputText", "text": err.Error()}},
		"success":      false,
	}, nil)
}
