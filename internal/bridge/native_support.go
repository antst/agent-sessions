package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
)

type nativePaths struct {
	profileKey    string
	codexHome     string
	appServerSock string
}

func resolveNativePaths() nativePaths {
	home, _ := os.UserHomeDir()
	codex := firstEnv("CODEX_HOME", filepath.Join(home, ".codex"))
	return nativePaths{
		profileKey:    sessionKey(codex),
		codexHome:     codex,
		appServerSock: firstEnv("AGENT_SESSIONS_CODEX_APP_SERVER_SOCKET", filepath.Join(codex, "app-server-control", "app-server-control.sock")),
	}
}

func firstEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func validSessionID(value string) bool {
	if len(value) < 1 || len(value) > 100 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

type appTurn struct {
	ID          string            `json:"id"`
	Status      any               `json:"status"`
	Items       []json.RawMessage `json:"items"`
	Error       any               `json:"error"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
	DurationMS  *int64            `json:"durationMs"`
}

type appThread struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"sessionId"`
	CreatedAt      int64     `json:"createdAt"`
	UpdatedAt      int64     `json:"updatedAt"`
	Cwd            string    `json:"cwd"`
	Name           string    `json:"name"`
	Path           string    `json:"path"`
	Source         any       `json:"source"`
	ParentThreadID string    `json:"parentThreadId"`
	Status         any       `json:"status"`
	Turns          []appTurn `json:"turns"`
}

func resumeThreadForPeer(client *appServerClient, threadID string) (*appThread, error) {
	var resumed struct {
		Thread appThread `json:"thread"`
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &resumed); err != nil {
		return nil, err
	}
	return &resumed.Thread, nil
}

func resumePreparedThread(client *appServerClient, threadID string, request map[string]any) (*appThread, string, error) {
	params := map[string]any{"threadId": threadID, "excludeTurns": true}
	if cwd := strings.TrimSpace(stringValue(request["cwd"])); cwd != "" {
		params["cwd"] = cwd
	}
	var resumed struct {
		Thread         appThread `json:"thread"`
		Cwd            string    `json:"cwd"`
		ApprovalPolicy any       `json:"approvalPolicy"`
	}
	if err := requestWithTimeout(client, 30*time.Second, "thread/resume", params, &resumed); err != nil {
		return nil, "", err
	}
	approvalPolicy := stringValue(resumed.ApprovalPolicy)
	if approvalPolicy == "" || strings.TrimSpace(resumed.Cwd) == "" {
		return nil, "", errors.New("thread/resume did not report its effective cwd and approval policy")
	}
	resumed.Thread.Cwd = strings.TrimSpace(resumed.Cwd)
	requestedApproval := strings.TrimSpace(stringValue(request["approvalPolicy"]))
	requestedSandbox := strings.TrimSpace(stringValue(request["sandbox"]))
	if err := updatePreparedThreadSettings(client, threadID, requestedApproval, requestedSandbox); err != nil {
		return nil, "", err
	}
	if requestedApproval != "" {
		approvalPolicy = requestedApproval
	}
	return &resumed.Thread, approvalPolicy, nil
}

func updatePreparedThreadSettings(client *appServerClient, threadID, approvalPolicy, sandbox string) error {
	if approvalPolicy == "" && sandbox == "" {
		return nil
	}
	settings := map[string]any{"threadId": threadID}
	putIf(settings, "approvalPolicy", approvalPolicy)
	if sandbox != "" {
		policy, err := sandboxPolicyForMode(sandbox)
		if err != nil {
			return err
		}
		settings["sandboxPolicy"] = policy
	}
	return requestWithTimeout(client, 30*time.Second, "thread/settings/update", settings, nil)
}

func sandboxPolicyForMode(mode string) (map[string]any, error) {
	switch mode {
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}, nil
	case "read-only":
		return map[string]any{"type": "readOnly"}, nil
	case "workspace-write":
		return map[string]any{"type": "workspaceWrite"}, nil
	default:
		return nil, fmt.Errorf("unsupported resumed Codex sandbox %q", mode)
	}
}

func listThreadMembership(client *appServerClient, archived bool) (map[string]bool, error) {
	threads := map[string]bool{}
	cursor := ""
	for len(threads) < 10000 {
		params := map[string]any{"archived": archived, "limit": 100, "sortDirection": "desc", "sortKey": "updated_at"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []appThread `json:"data"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := requestWithTimeout(client, 30*time.Second, "thread/list", params, &page); err != nil {
			return nil, err
		}
		for _, thread := range page.Data {
			if validSessionID(thread.ID) {
				threads[thread.ID] = true
			}
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return threads, nil
		}
		cursor = page.NextCursor
	}
	return threads, nil
}

func statusType(value any) string {
	switch current := value.(type) {
	case string:
		if current == "inProgress" {
			return "active"
		}
		return current
	case map[string]any:
		return stringValue(current["type"])
	}
	return ""
}

func rootThreadSource(source any) bool {
	value, ok := source.(string)
	return ok && (value == "cli" || value == "vscode" || value == "appServer")
}

func defaultPeerName(cwd, sessionID string) string {
	return sanitizeName("codex-" + canonicalBase(cwd) + "-" + first8(sessionID))
}

func classifyWakeMutationError(err error) error { return err }

func readLaneOutputSchema(file string) (json.RawMessage, error) {
	body, err := os.ReadFile(file) //nolint:gosec // caller explicitly selected the schema.
	if err != nil {
		return nil, err
	}
	if len(body) > maxFrameBytes {
		return nil, errors.New("output schema exceeds 1 MiB")
	}
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil || len(schema) == 0 {
		return nil, errors.New("output schema must be a non-empty JSON object")
	}
	compact, _ := json.Marshal(schema)
	if _, err := jsonschema.CompileString("lane-output-schema.json", string(compact)); err != nil {
		return nil, fmt.Errorf("compile output schema: %w", err)
	}
	return compact, nil
}

func listLaneTurns(client *appServerClient, threadID, itemsView string) ([]appTurn, error) {
	turns := []appTurn{}
	cursor := ""
	for len(turns) < 1000 {
		params := map[string]any{"threadId": threadID, "limit": 100, "sortDirection": "desc", "itemsView": itemsView}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []appTurn `json:"data"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := requestWithTimeout(client, 30*time.Second, "thread/turns/list", params, &page); err != nil {
			return nil, err
		}
		turns = append(turns, page.Data...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return turns, nil
		}
		cursor = page.NextCursor
	}
	return turns, nil
}

func normalizeStatus(value any) string {
	switch current := value.(type) {
	case string:
		return snakeCaseNative(current)
	case map[string]any:
		return snakeCaseNative(stringValue(current["type"]))
	}
	return ""
}

func snakeCaseNative(value string) string {
	var output strings.Builder
	for index, r := range value {
		if index > 0 && r >= 'A' && r <= 'Z' {
			output.WriteByte('_')
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		output.WriteRune(r)
	}
	return output.String()
}

func statusTerminal(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}
