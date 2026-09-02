package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// NativeLaneACPConfig describes one daemon-owned product-native ACP turn. The
// Agent Sessions daemon owns lifecycle and durability; only the vendor worker
// is a child process. This deliberately reuses the protocol clients exercised
// by the legacy Grok and Qwen lane managers instead of approximating their
// behavior through Grok's interactive/headless convenience CLI.
type NativeLaneACPConfig struct {
	Product          string
	Executable       string
	HostExecutable   string
	Cwd              string
	LaneID           string
	Capability       string
	NativeSessionID  string
	PermissionMode   string
	Model            string
	Effort           string
	Prompt           string
	Environment      []string
	SessionOpened    func(string) error
	ExecutionStarted func() error
	ExecutionTimeout time.Duration
}

// NativeLaneACPResult is the product-owned identity and final text observed on
// the ACP stream.
type NativeLaneACPResult struct {
	NativeSessionID string
	Output          string
	Mode            string
}

// RunNativeLaneACP executes one Grok turn over the product's supported ACP
// protocol and injects only the unified daemon's MCP connector.
func RunNativeLaneACP(ctx context.Context, config NativeLaneACPConfig) (NativeLaneACPResult, error) {
	switch config.Product {
	case "grok":
		return runGrokNativeLaneACP(ctx, config)
	default:
		return NativeLaneACPResult{}, fmt.Errorf("native ACP lane product %q is unsupported", config.Product)
	}
}

//nolint:gocyclo // Grok ACP bootstrap, exact identity, prompt, and teardown form one native transaction.
func runGrokNativeLaneACP(ctx context.Context, config NativeLaneACPConfig) (NativeLaneACPResult, error) {
	if err := validateNativeLaneACPConfig(config); err != nil {
		return NativeLaneACPResult{}, err
	}
	args := []string{"--no-auto-update", "agent", "--no-leader", "--always-approve"}
	if config.Model != "" {
		args = append(args, "--model", config.Model)
	}
	if config.Effort != "" {
		args = append(args, "--reasoning-effort", config.Effort)
	}
	args = append(args, "stdio")
	command := exec.Command(config.Executable, args...) //nolint:gosec // validated installed product and structured arguments.
	command.Dir, command.Env = config.Cwd, append([]string(nil), config.Environment...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return NativeLaneACPResult{}, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return NativeLaneACPResult{}, err
	}
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	worker, err := startGrokManagedProcess(command, nil)
	if err != nil {
		return NativeLaneACPResult{}, fmt.Errorf("start Grok ACP worker: %w", err)
	}
	client := newGrokACPClient(worker, stdin, stdout, config.LaneID, 0, nil)
	defer client.close()
	setupCtx, setupCancel := nativeLaneACPSetupContext(ctx)
	defer setupCancel()

	initialized, err := client.request(setupCtx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
	})
	if err != nil {
		return NativeLaneACPResult{}, nativeLaneACPError("initialize Grok ACP worker", err, diagnostics.String())
	}
	if !grokCachedTokenAdvertised(initialized) {
		return NativeLaneACPResult{}, errors.New("grok ACP worker did not advertise cached-token authentication")
	}
	if _, err := client.request(setupCtx, "authenticate", map[string]any{"methodId": "cached_token", "_meta": map[string]any{"headless": true}}); err != nil {
		return NativeLaneACPResult{}, nativeLaneACPError("authenticate Grok ACP worker", err, diagnostics.String())
	}
	server := unifiedLaneMCPServer(config)
	params := map[string]any{
		"cwd": config.Cwd, "mcpServers": []any{server},
		"_meta": map[string]any{"yoloMode": config.PermissionMode == "bypassPermissions"},
	}
	method := "session/new"
	if config.NativeSessionID != "" {
		method, params["sessionId"] = "session/load", config.NativeSessionID
	}
	opened, err := client.request(setupCtx, method, params)
	if err != nil {
		return NativeLaneACPResult{}, nativeLaneACPError("open Grok ACP lane session", err, diagnostics.String())
	}
	nativeID := defaultString(stringValue(opened["sessionId"]), config.NativeSessionID)
	if nativeID == "" {
		return NativeLaneACPResult{}, errors.New("grok ACP returned no native session identity")
	}
	if config.NativeSessionID != "" && nativeID != config.NativeSessionID {
		return NativeLaneACPResult{}, errors.New("grok ACP loaded a different native session identity")
	}
	if config.SessionOpened != nil {
		if err := config.SessionOpened(nativeID); err != nil {
			return NativeLaneACPResult{NativeSessionID: nativeID}, fmt.Errorf("persist Grok ACP session identity: %w", err)
		}
	}
	setupCancel()
	if config.ExecutionStarted != nil {
		if err := config.ExecutionStarted(); err != nil {
			return NativeLaneACPResult{NativeSessionID: nativeID}, fmt.Errorf("start Grok ACP lane execution: %w", err)
		}
	}
	var output strings.Builder
	var outputMu sync.Mutex
	client.setNotificationHandler(func(message map[string]any) {
		if method := stringValue(message["method"]); method != "session/update" && method != "x.ai/session/update" && method != "_x.ai/session/update" {
			return
		}
		params := mapValue(message["params"])
		if sessionID := stringValue(params["sessionId"]); sessionID != "" && sessionID != nativeID {
			return
		}
		update := mapValue(params["update"])
		if len(update) == 0 {
			update = mapValue(params["sessionUpdate"])
		}
		if stringValue(update["sessionUpdate"]) != "agent_message_chunk" {
			return
		}
		text := defaultString(stringValue(mapValue(update["content"])["text"]), stringValue(update["text"]))
		if text != "" {
			outputMu.Lock()
			output.WriteString(text)
			outputMu.Unlock()
		}
	})
	err = runNativeLaneACPExecution(ctx, config.ExecutionTimeout, func(turnCtx context.Context) error {
		_, requestErr := client.request(turnCtx, "session/prompt", map[string]any{
			"sessionId": nativeID, "prompt": []any{map[string]any{"type": "text", "text": config.Prompt}},
		})
		if requestErr != nil && turnCtx.Err() != nil {
			_ = client.notifyCancel(map[string]any{"sessionId": nativeID})
		}
		return requestErr
	})
	if err != nil {
		return NativeLaneACPResult{NativeSessionID: nativeID}, nativeLaneACPError("run Grok ACP lane turn", err, diagnostics.String())
	}
	outputMu.Lock()
	text := output.String()
	outputMu.Unlock()
	return NativeLaneACPResult{NativeSessionID: nativeID, Output: text, Mode: "bypassPermissions"}, nil
}

const nativeLaneACPSetupTimeout = 45 * time.Second

func nativeLaneACPSetupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, nativeLaneACPSetupTimeout)
}

func runNativeLaneACPExecution(parent context.Context, timeout time.Duration, execute func(context.Context) error) error {
	turnCtx := parent
	cancel := func() {}
	if timeout > 0 {
		turnCtx, cancel = context.WithTimeout(parent, timeout)
	}
	defer cancel()
	return execute(turnCtx)
}

func validateNativeLaneACPConfig(config NativeLaneACPConfig) error {
	if strings.TrimSpace(config.Executable) == "" || strings.TrimSpace(config.HostExecutable) == "" ||
		strings.TrimSpace(config.Cwd) == "" || strings.TrimSpace(config.LaneID) == "" ||
		strings.TrimSpace(config.Capability) == "" || strings.TrimSpace(config.Prompt) == "" {
		return errors.New("native ACP lane requires product, executables, cwd, identity, capability, and prompt")
	}
	return nil
}

func unifiedLaneMCPServer(config NativeLaneACPConfig) map[string]any {
	environment := map[string]string{
		"AGENT_SESSIONS_SESSION_ID":      config.LaneID,
		"AGENT_SESSIONS_PRODUCT":         config.Product,
		"AGENT_SESSIONS_LANE_CAPABILITY": config.Capability,
		"AGENT_SESSIONS_HOST_BINARY":     config.HostExecutable,
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]any, 0, len(names))
	for _, name := range names {
		values = append(values, map[string]any{"name": name, "value": environment[name]})
	}
	return map[string]any{
		"name": "agent_sessions", "command": config.HostExecutable,
		"args": []any{"connector", config.Product}, "env": values,
	}
}

func nativeLaneACPError(operation string, err error, diagnostics string) error {
	diagnostics = strings.TrimSpace(strings.Join(strings.Fields(strings.ToValidUTF8(diagnostics, "�")), " "))
	if len(diagnostics) > 512 {
		diagnostics = diagnostics[:512] + "…"
	}
	if diagnostics == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w; diagnostic: %s", operation, err, diagnostics)
}
