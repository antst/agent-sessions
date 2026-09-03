package bridge

import (
	"errors"
	"fmt"
	"strings"
)

func grokRosterStateFromResponse(response map[string]any, sessionID string) (grokRosterState, error) {
	result, _ := response["result"].(map[string]any)
	sessions, ok := result["sessions"].([]any)
	if !ok {
		return grokRosterState{}, fmt.Errorf("%w: response has no sessions", errGrokRosterAuthorityLost)
	}
	state, matches, err := grokRosterStateFromRows(sessions, sessionID)
	if err != nil {
		return grokRosterState{}, fmt.Errorf("%w: %w", errGrokRosterAuthorityLost, err)
	}
	if matches != 1 {
		return grokRosterState{}, fmt.Errorf("%w: roster returned %d exact rows for %s", errGrokRosterAuthorityLost, matches, sessionID)
	}
	if state.authorityLost {
		return grokRosterState{}, fmt.Errorf("%w: actor %s is not live", errGrokRosterAuthorityLost, sessionID)
	}
	return state, nil
}

func grokRosterNotificationState(message map[string]any, sessionID string) (grokRosterState, bool) {
	params, _ := message["params"].(map[string]any)
	if nested, ok := params["params"].(map[string]any); ok {
		if method := stringValue(params["method"]); method != "" && method != "x.ai/sessions/changed" {
			return grokRosterState{}, false
		}
		params = nested
	}
	if removed, ok := params["removed"].([]any); ok {
		for _, raw := range removed {
			if stringValue(raw) == sessionID {
				return grokRosterState{fromPush: true, authorityLost: true}, true
			}
		}
	}
	rows, ok := params["upserted"].([]any)
	if !ok {
		return grokRosterState{}, false
	}
	state, matches, err := grokRosterStateFromRows(rows, sessionID)
	state.fromPush = true
	if matches == 0 {
		return grokRosterState{}, false
	}
	if err != nil || matches != 1 {
		return grokRosterState{fromPush: true, authorityLost: true}, true
	}
	return state, true
}

func grokRosterStateFromRows(rows []any, sessionID string) (grokRosterState, int, error) {
	matches := 0
	state := grokRosterState{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if stringValue(row["sessionId"]) != sessionID {
			continue
		}
		matches++
		resident, residentOK := row["resident"].(bool)
		if !residentOK {
			return grokRosterState{}, matches, errors.New("exact Grok session roster row has no resident state")
		}
		activity := stringValue(row["activity"])
		if !resident || activity == "completed" || activity == "dormant" || activity == "dead" {
			state.authorityLost = true
			continue
		}
		yolo, yoloOK := row["yolo"].(bool)
		if !yoloOK {
			return grokRosterState{}, matches, errors.New("live Grok session roster row has no yolo state")
		}
		status, err := grokRosterActivityStatus(activity)
		if err != nil {
			return grokRosterState{}, matches, err
		}
		state = grokRosterState{
			name:           strings.TrimSpace(stringValue(row["title"])),
			permissionMode: map[bool]string{true: "bypassPermissions", false: "default"}[yolo],
			status:         status,
		}
	}
	return state, matches, nil
}

func grokRosterTitleFromRows(rows []any, sessionID string) (string, int) {
	name, matches := "", 0
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if stringValue(row["sessionId"]) != sessionID {
			continue
		}
		matches++
		name = strings.TrimSpace(stringValue(row["title"]))
	}
	return name, matches
}

func grokRosterActivityStatus(activity string) (string, error) {
	switch activity {
	case "working":
		return "busy", nil
	case "needs_input":
		return "waiting", nil
	case "idle":
		return "idle", nil
	default:
		return "", fmt.Errorf("live Grok session roster row has unsupported activity %q", activity)
	}
}

func grokCachedTokenAdvertised(result map[string]any) bool {
	methods, _ := result["authMethods"].([]any)
	for _, raw := range methods {
		method, _ := raw.(map[string]any)
		if stringValue(method["id"]) == "cached_token" {
			return true
		}
	}
	return false
}

func grokAgentSessionsMCPIdentityReady(response map[string]any, sessionID string) error {
	result, _ := response["result"].(map[string]any)
	if isError, _ := result["isError"].(bool); isError {
		return errors.New("grok agent_sessions MCP readiness tool returned an error")
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return errors.New("grok agent_sessions MCP readiness tool returned no content")
	}
	identity, _ := result["structuredContent"].(map[string]any)
	if len(identity) != 0 && stringValue(identity["sessionId"]) != sessionID {
		return errors.New("grok agent_sessions MCP readiness tool returned the wrong session identity")
	}
	return nil
}
