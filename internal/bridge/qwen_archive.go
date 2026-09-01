package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/qwenprofile"
)

var qwenLaneServeAddressPattern = regexp.MustCompile(`qwen serve listening on (http://127\.0\.0\.1:[0-9]+)`)

var executeQwenArchiveTransaction = runQwenArchiveTransaction

type qwenArchiveResponse struct {
	Archived        []string `json:"archived"`
	AlreadyArchived []string `json:"alreadyArchived"`
	Unarchived      []string `json:"unarchived"`
	AlreadyActive   []string `json:"alreadyActive"`
	NotFound        []string `json:"notFound"`
	Errors          []struct {
		SessionID string `json:"sessionId"`
		Error     string `json:"error"`
	} `json:"errors"`
}

// runQwenArchiveTransaction uses Qwen's supported private serve control plane
// only after the ACP writer has retired. The helper is a bounded private
// process group and is always reaped together with preheated descendants.
func runQwenArchiveTransaction(state qwenLaneState, operation string) error { //nolint:gocyclo // One bounded native helper transaction owns start, auth, request, and exact retirement.
	if operation != "archive" && operation != "unarchive" {
		return fmt.Errorf("unsupported Qwen archive operation %q", operation)
	}
	if !validSessionID(state.QwenSessionID) || state.Cwd == "" {
		return errors.New("qwen lane has no exact native session identity to archive")
	}
	if state.WorkerPID > 1 && exactProcessIdentityMatch(state.WorkerPID, state.WorkerProcStart) {
		return errors.New("refuse Qwen native archive while its ACP worker is live")
	}
	executable := strings.TrimSpace(os.Getenv(qwenLaneExecutableEnv))
	if executable == "" {
		var err error
		executable, err = exec.LookPath("qwen")
		if err != nil {
			return errors.New("validated Qwen executable is unavailable for archive")
		}
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("create Qwen archive capability: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "serve", "--port", "0", "--hostname", "127.0.0.1", "--require-auth", "--workspace", state.Cwd, "--no-web") //nolint:gosec // admitted executable and structured private-helper argv.
	command.Dir = state.Cwd
	command.Env = qwenArchiveEnvironment(os.Environ(), state.Profile, token)
	reader, writer := io.Pipe()
	command.Stdout, command.Stderr = writer, writer
	process, err := startGrokManagedProcess(command, nil)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return fmt.Errorf("start Qwen archive helper: %w", err)
	}
	defer func() {
		stopGrokManagedProcess(process, 3*time.Second)
		_ = writer.Close()
		_ = reader.Close()
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	address := ""
	for scanner.Scan() {
		if match := qwenLaneServeAddressPattern.FindStringSubmatch(scanner.Text()); len(match) == 2 {
			address = match[1]
			break
		}
	}
	if address == "" {
		return errors.New("qwen archive helper did not publish a private loopback address")
	}
	go func() {
		for scanner.Scan() {
		}
	}()
	if err := qwenValidateArchiveCapabilities(ctx, address, token, state.Cwd); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"sessionIds": []string{state.QwenSessionID}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address+"/sessions/"+operation, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request Qwen native %s: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("qwen native %s returned %s", operation, response.Status)
	}
	var result qwenArchiveResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&result); err != nil {
		return fmt.Errorf("decode Qwen native %s result: %w", operation, err)
	}
	if len(result.NotFound) != 0 || len(result.Errors) != 0 {
		return fmt.Errorf("qwen native %s did not confirm exact session %s", operation, state.QwenSessionID)
	}
	success := result.Archived
	already := result.AlreadyArchived
	if operation == "unarchive" {
		success, already = result.Unarchived, result.AlreadyActive
	}
	if !exactSingleQwenArchiveResult(success, already, state.QwenSessionID) {
		return fmt.Errorf("qwen native %s returned an ambiguous result", operation)
	}
	return nil
}

func qwenValidateArchiveCapabilities(ctx context.Context, address, token, cwd string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/capabilities", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("qwen archive helper capabilities returned %s", response.Status)
	}
	var capabilities struct {
		ProtocolVersions struct {
			Current string `json:"current"`
		} `json:"protocolVersions"`
		Features  []string `json:"features"`
		Workspace string   `json:"workspaceCwd"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&capabilities); err != nil {
		return err
	}
	if capabilities.ProtocolVersions.Current != "v1" || capabilities.Workspace != cwd || !containsString(capabilities.Features, "session_archive") {
		return errors.New("qwen archive helper has the wrong protocol, workspace, or capability")
	}
	return nil
}

func exactSingleQwenArchiveResult(success, already []string, sessionID string) bool {
	return len(success)+len(already) == 1 && (len(success) == 1 && success[0] == sessionID || len(already) == 1 && already[0] == sessionID)
}

func qwenArchiveEnvironment(environment []string, profile qwenprofile.Identity, token string) []string {
	blocked := map[string]bool{
		"QWEN_SERVER_TOKEN": true, qwenLaneLaunchTokenEnv: true,
		"AGENT_SESSIONS_QWEN_CAPABILITY": true, "AGENT_SESSIONS_REMOTE_PARENT_CONTEXT": true,
		"AGENT_SESSIONS_SESSION_ID": true, "AGENT_SESSIONS_PRODUCT": true,
	}
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if !blocked[name] {
			filtered = append(filtered, entry)
		}
	}
	return append(qwenprofile.ApplyEnvironment(filtered, profile), "QWEN_SERVER_TOKEN="+token)
}

func completeQwenLaneArchive(paths nativePaths, state qwenLaneState, reason string) error {
	if state.WorkerPID > 1 && exactProcessIdentityMatch(state.WorkerPID, state.WorkerProcStart) {
		return errors.New("qwen lane ACP worker is still live")
	}
	state.Status, state.NativeArchiveState = "retiring", "archiving"
	state.CleanupDebt = nil
	if err := writeQwenLaneState(paths, state); err != nil {
		return err
	}
	if err := executeQwenArchiveTransaction(state, "archive"); err != nil {
		state.CleanupDebt = []qwenLaneDebt{{Operation: "archive", Error: err.Error(), Attempts: 1, UpdatedAt: time.Now().UnixMilli()}}
		state.Status = "cleanup_debt"
		_ = writeQwenLaneState(paths, state)
		return fmt.Errorf("%s: %w", reason, err)
	}
	state.Status, state.NativeArchiveState = "archived", "archived"
	state.ManagerPID, state.ManagerProcStart, state.ManagerStrongStart = 0, "", ""
	state.WorkerPID, state.WorkerProcStart, state.WorkerStrongStart = 0, "", ""
	state.ToolRegistryVersion, state.ToolWrapperPath, state.ToolRealBash = 0, "", ""
	state.ControlSocket, state.MessagingSocket, state.StartupID = "", "", ""
	state.ActiveTurnID, state.PendingTurnIDs, state.AutoArchiveAt = "", nil, 0
	state.CleanupDebt = nil
	return writeQwenLaneState(paths, state)
}
