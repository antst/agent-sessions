package qwenreadiness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const nativeProbeTimeout = 12 * time.Second

var qwenServeAddressPattern = regexp.MustCompile(`qwen serve listening on (http://127\.0\.0\.1:[0-9]+)`)

// NativeSource probes one installed Qwen CLI and selected profile without
// opening, resuming, or prompting a native session.
type NativeSource struct {
	environment []string

	mu       sync.Mutex
	serveKey string
	serve    nativeServeEvidence
	serveErr error
}

type nativeServeEvidence struct {
	version      string
	workspace    string
	capabilities []string
	trusted      bool
	auth         State
	authStatus   string
}

// NewNativeSource returns the production readiness evidence source. The
// supplied environment is copied and filtered through the selected profile by
// every native child.
func NewNativeSource(environment []string) *NativeSource {
	return &NativeSource{environment: append([]string(nil), environment...)}
}

// InspectExecutable resolves and identifies the selected official Qwen package.
func (s *NativeSource) InspectExecutable(ctx context.Context, executable string) (ExecutableEvidence, error) {
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return ExecutableEvidence{}, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return ExecutableEvidence{}, err
	}
	versionBody, err := runNativeProbe(ctx, s.environment, executable, "--version")
	if err != nil {
		return ExecutableEvidence{}, fmt.Errorf("run qwen --version: %w", err)
	}
	version := strings.TrimSpace(string(versionBody))
	packageName, packageVersion := inspectQwenPackageIdentity(resolved)
	if packageName == "" {
		return ExecutableEvidence{}, errors.New("cannot identify selected Qwen package")
	}
	if packageVersion != version {
		return ExecutableEvidence{}, fmt.Errorf("qwen package version %q does not match executable %q", packageVersion, version)
	}
	return ExecutableEvidence{
		Executable: executable, ResolvedExecutable: resolved,
		Package: packageName, Version: version,
	}, nil
}

// ProbeParser exercises presence-sensitive Qwen CLI parser contracts without creating a session.
func (s *NativeSource) ProbeParser(ctx context.Context, executable string, probe ParserProbe) (State, error) {
	base := []string{}
	if probe.ApprovalMode.Set {
		if probe.ApprovalMode.Value == "" {
			return StateUnready, errors.New("approval mode was present but empty")
		}
		base = append(base, "--approval-mode", probe.ApprovalMode.Value)
	}
	if probe.Contract != ParserDualOutput {
		output, err := runNativeProbeExpectFailure(ctx, s.environment, executable, append(base, "--session-id", "not-a-uuid")...)
		if err != nil {
			return StateUnready, err
		}
		if !strings.Contains(strings.ToLower(string(output)), "uuid") {
			return StateUnready, errors.New("qwen parser did not reach exact session UUID validation")
		}
		return StateReady, nil
	}
	probes := [][]string{
		{"--session-id", "not-a-uuid"},
		{"--resume", "not-a-uuid"},
		{"--chat-recording=true", "--session-id", "not-a-uuid"},
		{"--input-file", os.DevNull, "--session-id", "not-a-uuid"},
		{"--mcp-config", "{}"},
		{"--json-file", filepath.Join(os.TempDir(), "agent-sessions-qwen-readiness.jsonl"), "--json-fd", "3"},
	}
	markers := []string{"uuid", "uuid", "uuid", "uuid", "no input provided", "mutually exclusive"}
	for index, args := range probes {
		output, err := runNativeProbeExpectFailure(ctx, s.environment, executable, args...)
		if err != nil {
			return StateUnready, err
		}
		if !strings.Contains(strings.ToLower(string(output)), markers[index]) {
			return StateUnready, fmt.Errorf("qwen parser probe %q did not contain %q: %s", args, markers[index], strings.TrimSpace(string(output)))
		}
	}
	return StateReady, nil
}

// InitializeACP performs only the Qwen ACP initialize handshake and records its advertised contract.
func (s *NativeSource) InitializeACP(ctx context.Context, executable string, profile qwenprofile.Identity) (ACPEvidence, error) {
	probeCtx, cancel := context.WithTimeout(ctx, nativeProbeTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, executable, "--acp")
	command.Env = qwenProbeEnvironment(s.environment, profile)
	stdin, err := command.StdinPipe()
	if err != nil {
		return ACPEvidence{}, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return ACPEvidence{}, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return ACPEvidence{}, err
	}
	request := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		}},
	}
	if err := json.NewEncoder(stdin).Encode(request); err != nil {
		_ = command.Process.Kill()
		return ACPEvidence{}, err
	}
	_ = stdin.Close()
	var response struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(stdout, 2<<20)).Decode(&response)
	_ = command.Process.Signal(syscall.SIGTERM)
	_ = command.Wait()
	if decodeErr != nil {
		return ACPEvidence{}, fmt.Errorf("decode Qwen ACP initialize: %w: %s", decodeErr, strings.TrimSpace(stderr.String()))
	}
	if response.Error != nil || response.Result == nil {
		return ACPEvidence{}, errors.New("qwen ACP initialize returned an error")
	}
	agent, _ := response.Result["agentInfo"].(map[string]any)
	capabilities, _ := response.Result["agentCapabilities"].(map[string]any)
	sessions, _ := capabilities["sessionCapabilities"].(map[string]any)
	mcp, _ := capabilities["mcpCapabilities"].(map[string]any)
	return ACPEvidence{
		ProtocolVersion: intNumber(response.Result["protocolVersion"]),
		AgentName:       stringNumber(agent["name"]), AgentVersion: stringNumber(agent["version"]),
		LoadSession:  boolNumber(capabilities["loadSession"]),
		ListSessions: sessions["list"] != nil, ResumeSession: sessions["resume"] != nil,
		MCP: len(mcp) != 0,
	}, nil
}

// ProbeArchive verifies the native archive control surface without mutating a session.
func (s *NativeSource) ProbeArchive(ctx context.Context, executable, workspace string, profile qwenprofile.Identity) (ArchiveEvidence, error) {
	evidence, err := s.inspectServe(ctx, executable, workspace, profile)
	if err != nil {
		return ArchiveEvidence{}, err
	}
	return ArchiveEvidence{
		ProtocolVersion: "v1", QwenVersion: evidence.version,
		Workspace: evidence.workspace, Capabilities: append([]string(nil), evidence.capabilities...),
	}, nil
}

// InspectTrust reports whether the exact selected workspace is trusted by Qwen.
func (s *NativeSource) InspectTrust(ctx context.Context, executable, workspace string, profile qwenprofile.Identity) (State, error) {
	evidence, err := s.inspectServe(ctx, executable, workspace, profile)
	if err != nil {
		return StateUnknown, err
	}
	if evidence.trusted {
		return StateReady, nil
	}
	return StateUnready, nil
}

// InspectCredentialConfiguration reports the credential state returned by the admitted native preflight.
func (s *NativeSource) InspectCredentialConfiguration(_ context.Context, _ qwenprofile.Identity) (State, error) {
	s.mu.Lock()
	evidence, err := s.serve, s.serveErr
	s.mu.Unlock()
	if err != nil {
		return StateUnknown, err
	}
	if evidence.workspace == "" {
		return StateUnknown, errors.New("qwen credential probe requires the archive/trust probe first")
	}
	return evidence.auth, nil
}

// InspectIntegration attests the installed Agent Sessions extension inventory for the selected profile.
func (s *NativeSource) InspectIntegration(_ context.Context, profile qwenprofile.Identity) (IntegrationEvidence, error) {
	home, err := qwenprofile.EffectiveHome(profile, envutil.Lookup(s.environment))
	if err != nil {
		return IntegrationEvidence{}, err
	}
	root := filepath.Join(home, "extensions", "agent-sessions")
	manifest, manifestBody, err := readStrictJSONObject(filepath.Join(root, "plugin.json"))
	if err != nil {
		return IntegrationEvidence{}, err
	}
	mcp, mcpBody, err := readStrictJSONObject(filepath.Join(root, "mcp.json"))
	if err != nil {
		return IntegrationEvidence{}, err
	}
	servers, _ := mcp["mcpServers"].(map[string]any)
	server, _ := servers["agent_sessions"].(map[string]any)
	ready := stringNumber(manifest["name"]) == "agent-sessions" &&
		stringNumber(server["type"]) == "stdio" && stringNumber(server["command"]) != ""
	enabled, stateErr := qwenIntegrationEnabled(filepath.Join(home, "extension-store", "state.json"))
	if stateErr != nil {
		return IntegrationEvidence{}, stateErr
	}
	digest := sha256.Sum256(append(append([]byte(nil), manifestBody...), mcpBody...))
	return IntegrationEvidence{
		ID: "agent-sessions", Version: stringNumber(manifest["version"]),
		ManifestDigest: hex.EncodeToString(digest[:]), ProfileFingerprint: profile.Fingerprint,
		Ready: ready && enabled,
	}, nil
}

func (s *NativeSource) inspectServe(ctx context.Context, executable, workspace string, profile qwenprofile.Identity) (nativeServeEvidence, error) {
	key := executable + "\x00" + workspace + "\x00" + profile.Fingerprint
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveKey == key {
		return s.serve, s.serveErr
	}
	s.serveKey = key
	s.serve, s.serveErr = probeQwenServe(ctx, s.environment, executable, workspace, profile)
	return s.serve, s.serveErr
}

func probeQwenServe(ctx context.Context, environment []string, executable, workspace string, profile qwenprofile.Identity) (nativeServeEvidence, error) { //nolint:gocyclo // Ephemeral authenticated daemon startup and three bounded read-only requests form one probe.
	probeCtx, cancel := context.WithTimeout(ctx, nativeProbeTimeout)
	defer cancel()
	tokenBytes := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", os.Getpid(), workspace)))
	token := hex.EncodeToString(tokenBytes[:])
	command := exec.CommandContext(probeCtx, executable, "serve", "--port", "0", "--hostname", "127.0.0.1", "--require-auth", "--workspace", workspace, "--no-web") //nolint:gosec // admitted executable and fixed session-free serve argv.
	command.Env = append(qwenProbeEnvironment(environment, profile), "QWEN_SERVER_TOKEN="+token)
	reader, writer := io.Pipe()
	command.Stdout, command.Stderr = writer, writer
	if err := command.Start(); err != nil {
		return nativeServeEvidence{}, err
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		_ = writer.Close()
	}()
	defer func() {
		if command.Process != nil {
			_ = command.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			<-done
		}
		_ = reader.Close()
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	address := ""
	for scanner.Scan() {
		if match := qwenServeAddressPattern.FindStringSubmatch(scanner.Text()); len(match) == 2 {
			address = match[1]
			break
		}
	}
	if address == "" {
		return nativeServeEvidence{}, errors.New("qwen serve did not publish a private loopback address")
	}
	// Keep draining daemon diagnostics. os/exec waits for its pipe copier; if
	// the reader stops after the listening line, shutdown can deadlock once the
	// bounded pipe fills.
	go func() {
		for scanner.Scan() {
		}
	}()
	get := func(path string, output any) error {
		request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, address+path, nil)
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
			return fmt.Errorf("qwen serve %s returned %s", path, response.Status)
		}
		return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output)
	}
	var capabilities struct {
		ProtocolVersions struct {
			Current string `json:"current"`
		} `json:"protocolVersions"`
		QwenVersion string   `json:"qwenCodeVersion"`
		Features    []string `json:"features"`
		Workspace   string   `json:"workspaceCwd"`
		Workspaces  []struct {
			Cwd     string `json:"cwd"`
			Trusted bool   `json:"trusted"`
		} `json:"workspaces"`
	}
	if err := get("/capabilities", &capabilities); err != nil {
		return nativeServeEvidence{}, err
	}
	var trust struct {
		Workspace string `json:"workspaceCwd"`
		Effective struct {
			State string `json:"state"`
		} `json:"effective"`
	}
	if err := get("/workspace/trust", &trust); err != nil {
		return nativeServeEvidence{}, err
	}
	var preflight struct {
		Workspace   string `json:"workspaceCwd"`
		Initialized bool   `json:"initialized"`
		Cells       []struct {
			Kind, Status string
		} `json:"cells"`
	}
	for {
		if err := get("/workspace/preflight", &preflight); err != nil {
			return nativeServeEvidence{}, err
		}
		hasAuth := false
		authSettled := false
		for _, cell := range preflight.Cells {
			if cell.Kind == "auth" {
				hasAuth = true
				authSettled = cell.Status != "not_started"
			}
		}
		if preflight.Initialized && hasAuth && authSettled {
			break
		}
		select {
		case <-probeCtx.Done():
			return nativeServeEvidence{}, errors.New("qwen preflight did not initialize before readiness deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	auth := StateUnknown
	authStatus := "missing"
	for _, cell := range preflight.Cells {
		if cell.Kind == "auth" {
			authStatus = cell.Status
			if cell.Status == "ok" {
				auth = StateReady
			} else {
				auth = StateUnready
			}
		}
	}
	trusted := trust.Workspace == workspace && trust.Effective.State == "trusted"
	return nativeServeEvidence{
		version: capabilities.QwenVersion, workspace: capabilities.Workspace,
		capabilities: capabilities.Features, trusted: trusted, auth: auth,
		authStatus: authStatus,
	}, nil
}

func runNativeProbe(ctx context.Context, environment []string, executable string, args ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, nativeProbeTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, executable, args...) //nolint:gosec // operator-selected executable and bounded parser argv.
	command.Env = append(append([]string(nil), environment...), "QWEN_CODE_NO_RELAUNCH=1")
	return command.CombinedOutput()
}

func runNativeProbeExpectFailure(ctx context.Context, environment []string, executable string, args ...string) ([]byte, error) {
	output, err := runNativeProbe(ctx, environment, executable, args...)
	if err == nil {
		return output, errors.New("qwen parser probe unexpectedly succeeded")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return output, err
	}
	return output, nil
}

func qwenProbeEnvironment(environment []string, profile qwenprofile.Identity) []string {
	result := qwenprofile.ApplyEnvironment(environment, profile)
	return append(result, "QWEN_CODE_NO_RELAUNCH=1")
}

func inspectQwenPackageIdentity(executable string) (string, string) {
	candidates := []string{executable}
	if body, err := os.ReadFile(executable); err == nil && len(body) <= 64*1024 { //nolint:gosec // executable was resolved from the operator-selected Qwen command.
		for _, field := range strings.Fields(string(body)) {
			field = strings.Trim(field, "'\"`;$")
			if filepath.IsAbs(field) && filepath.Base(field) == "qwen" {
				candidates = append(candidates, field)
			}
		}
	}
	for _, candidate := range candidates {
		for directory := filepath.Dir(candidate); directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
			value, _, err := readStrictJSONObject(filepath.Join(directory, "package.json"))
			if err == nil && stringNumber(value["name"]) == ExpectedPackage {
				return stringNumber(value["name"]), stringNumber(value["version"])
			}
		}
	}
	return "", ""
}

func readStrictJSONObject(path string) (map[string]any, []byte, error) {
	info, err := os.Lstat(path) //nolint:gosec // caller supplies an exact profile/package metadata path; type and size are checked before reading.
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4<<20 {
		return nil, nil, fmt.Errorf("qwen readiness file is not a bounded regular file: %s", path)
	}
	body, err := os.ReadFile(path) //nolint:gosec // exact bounded profile-owned metadata, never credential values.
	if err != nil {
		return nil, nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, nil, err
	}
	return value, body, nil
}

func qwenIntegrationEnabled(path string) (bool, error) {
	state, _, err := readStrictJSONObject(path)
	if err != nil {
		return false, err
	}
	extensions, _ := state["extensions"].(map[string]any)
	matches := 0
	enabled := false
	for _, raw := range extensions {
		entry, _ := raw.(map[string]any)
		if stringNumber(entry["name"]) == "agent-sessions" {
			matches++
			enabled = stringNumber(entry["defaultActivation"]) == "enabled"
		}
	}
	if matches != 1 {
		return false, fmt.Errorf("qwen extension store contains %d agent-sessions entries", matches)
	}
	return enabled, nil
}

func stringNumber(value any) string { value, _ = value.(string); return fmt.Sprint(value) }
func intNumber(value any) int       { number, _ := value.(float64); return int(number) }
func boolNumber(value any) bool     { result, _ := value.(bool); return result }
