package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/federator"
)

const (
	maxMCPLaneArgumentBytes = 512 * 1024
	maxMCPLaneOutputBytes   = 192 * 1024
	mcpLaneShortTimeout     = 5 * time.Minute
	mcpLaneMaximumTimeout   = 24 * time.Hour
	mcpLaneTimeoutMargin    = time.Minute
)

var mcpLaneCommands = map[string]bool{
	"doctor": true, "list": true, "run": true, "start": true, "resume": true,
	"wait": true, "status": true, "interrupt": true, "archive": true,
}

var mcpLaneRuntimeExecutable = os.Executable

type mcpLaneRequest struct {
	product string
	role    string
	command string
	args    []string
	input   string
	host    string
}

func callMCPParentLane(paths nativePaths, args map[string]any, callerSessionID string) (map[string]any, error) {
	sessionID, err := requireMCPCallerSession(paths, args, callerSessionID)
	if err != nil {
		return nil, err
	}
	request, err := parseMCPParentLaneRequest(args)
	if err != nil {
		return nil, err
	}
	runtimeDir, err := requireGroupedAgentRuntime(paths, sessionID)
	if err != nil {
		return nil, err
	}
	parent, err := federator.ResolveParentContext(runtimeDir, sessionID)
	if err != nil {
		return nil, fmt.Errorf("resolve lane parent context: %w", err)
	}

	commandTimeout, err := mcpLaneRequestTimeout(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	stdout, stderr := &mcpCappedBuffer{limit: maxMCPLaneOutputBytes}, &mcpCappedBuffer{limit: maxMCPLaneOutputBytes}
	exitCode, runErr := runMCPParentLaneRequest(ctx, paths, runtimeDir, sessionID, parent, request, stdout, stderr)
	proxyTimedOut := ctx.Err() != nil
	if proxyTimedOut {
		exitCode, runErr = 124, ctx.Err()
	}
	if runErr != nil && exitCode == 0 {
		exitCode = 1
	}
	return mcpLaneResult(request, exitCode, runErr, proxyTimedOut, stdout, stderr), nil
}

func parseMCPParentLaneRequest(args map[string]any) (mcpLaneRequest, error) {
	request := mcpLaneRequest{product: strings.TrimSpace(stringValue(args["product"]))}
	product, ok := bridgeProductByID(request.product)
	if !ok {
		return mcpLaneRequest{}, errors.New("lane product is not supported")
	}
	request.role = product.descriptor.LaneRuntimeRole
	request.command = strings.TrimSpace(stringValue(args["command"]))
	if !mcpLaneCommands[request.command] {
		return mcpLaneRequest{}, errors.New("unsupported lane lifecycle command")
	}
	nativeArgs, err := mcpLaneArguments(args["arguments"])
	if err != nil {
		return mcpLaneRequest{}, err
	}
	request.args = append([]string{request.command}, nativeArgs...)
	request.input = stringValue(args["input"])
	if len(request.input) > maxFrameBytes {
		return mcpLaneRequest{}, fmt.Errorf("lane stdin exceeds %d bytes", maxFrameBytes)
	}
	if request.input != "" && request.command != "run" && request.command != "start" && request.command != "resume" {
		return mcpLaneRequest{}, errors.New("lane input is valid only for run, start, and resume")
	}
	request.host = strings.TrimSpace(stringValue(args["host"]))
	return request, nil
}

func mcpLaneRequestTimeout(request mcpLaneRequest) (time.Duration, error) {
	if request.command != "run" && request.command != "resume" && request.command != "wait" {
		return mcpLaneShortTimeout, nil
	}
	seconds, specified, err := mcpLaneTimeoutArgument(request.args[1:])
	if err != nil {
		return 0, err
	}
	if !specified || seconds == 0 {
		return mcpLaneMaximumTimeout, nil
	}
	maximumSeconds := float64((mcpLaneMaximumTimeout - mcpLaneTimeoutMargin) / time.Second)
	if seconds > maximumSeconds {
		return 0, fmt.Errorf("lane %s --timeout exceeds the MCP maximum of %d seconds; use start and bounded wait calls",
			request.command, int(maximumSeconds))
	}
	duration := time.Duration(seconds * float64(time.Second))
	return duration + mcpLaneTimeoutMargin, nil
}

func mcpLaneTimeoutArgument(arguments []string) (float64, bool, error) {
	seconds, specified := float64(0), false
	for index := 0; index < len(arguments); index++ {
		if arguments[index] != "--timeout" {
			continue
		}
		if index+1 >= len(arguments) {
			return 0, false, errors.New("lane --timeout requires a value")
		}
		value, err := strconv.ParseFloat(arguments[index+1], 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
			return 0, false, errors.New("lane --timeout must be a non-negative number of seconds")
		}
		seconds, specified = value, true
		index++
	}
	return seconds, specified, nil
}

func runMCPParentLaneRequest(
	ctx context.Context,
	paths nativePaths,
	runtimeDir, sessionID string,
	parent federator.ParentContext,
	request mcpLaneRequest,
	stdout, stderr *mcpCappedBuffer,
) (int, error) {
	if request.host != "" {
		return federator.RunRemoteLane(ctx, federator.RemoteLaneOptions{
			RuntimeDir: runtimeDir, Host: request.host, Product: request.product,
			SourceSession: sessionID, Args: request.args,
		}, strings.NewReader(request.input), stdout, stderr)
	}
	return runLocalMCPParentLane(
		ctx, paths, runtimeDir, parent, request.role, request.args, request.input, stdout, stderr,
	)
}

func mcpLaneResult(
	request mcpLaneRequest,
	exitCode int,
	runErr error,
	proxyTimedOut bool,
	stdout, stderr *mcpCappedBuffer,
) map[string]any {
	data := map[string]any{
		"product": request.product, "command": request.command, "host": emptyStringAsNil(request.host), "exit": exitCode,
		"stdout": stdout.String(), "stderr": stderr.String(),
		"stdout_truncated": stdout.truncated, "stderr_truncated": stderr.truncated,
		"proxy_timeout": proxyTimedOut,
	}
	if runErr != nil {
		data["error"] = runErr.Error()
	}
	text := fmt.Sprintf("%s lane %s exited %d.", request.product, request.command, exitCode)
	if stdout.Len() != 0 {
		text += "\nstdout:\n" + stdout.String()
	}
	if stderr.Len() != 0 {
		text += "\nstderr:\n" + stderr.String()
	}
	if runErr != nil && stderr.Len() == 0 {
		text += "\nerror: " + runErr.Error()
	}
	return map[string]any{"text": text, "data": data}
}

func runLocalMCPParentLane(
	ctx context.Context,
	paths nativePaths,
	runtimeDir string,
	parent federator.ParentContext,
	role string,
	args []string,
	input string,
	stdout, stderr *mcpCappedBuffer,
) (int, error) {
	executable, err := mcpLaneRuntimeExecutable()
	if err != nil {
		return 1, err
	}
	command := exec.CommandContext(ctx, executable, append([]string{role}, args...)...) //nolint:gosec // exact runtime and validated argv vector.
	command.Env = envutil.Replace(os.Environ(), map[string]string{
		"AGENT_SESSIONS_AGENT_RUNTIME_DIR":     runtimeDir,
		"AGENT_SESSIONS_REMOTE_PARENT_CONTEXT": "",
		"AGENT_SESSIONS_SESSION_ID":            parent.SessionID,
		"AGENT_SESSIONS_PRODUCT":               parent.Product,
		"CLAUDE_PEER_DATA_DIR":                 paths.dataRoot,
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR":        paths.claudeRoot,
		"CODEX_HOME":                           paths.codexHome,
		"CODEX_THREAD_ID":                      parent.SessionID,
		"GROK_PEER_NATIVE_RUNTIME":             executable,
	})
	command.Stdin, command.Stdout, command.Stderr = strings.NewReader(input), stdout, stderr
	err = command.Run()
	if err == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode(), err
	}
	return 1, err
}

func mcpLaneArguments(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]string); typedOK {
			values = make([]any, len(typed))
			for index := range typed {
				values[index] = typed[index]
			}
		} else {
			return nil, errors.New("lane arguments must be an array of strings")
		}
	}
	if len(values) > 256 {
		return nil, errors.New("lane arguments exceed 256 entries")
	}
	result, total := make([]string, 0, len(values)), 0
	for _, value := range values {
		argument, ok := value.(string)
		if !ok || strings.ContainsRune(argument, '\x00') {
			return nil, errors.New("lane arguments must contain valid strings")
		}
		total += len(argument)
		if len(argument) > 4096 || total > maxMCPLaneArgumentBytes {
			return nil, errors.New("lane arguments exceed the size limit")
		}
		result = append(result, argument)
	}
	return result, nil
}

type mcpCappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *mcpCappedBuffer) Write(body []byte) (int, error) {
	written := len(body)
	if b.limit <= 0 {
		b.truncated = true
		return written, nil
	}
	if len(body) >= b.limit {
		b.Reset()
		_, _ = b.Buffer.Write(body[len(body)-b.limit:])
		b.truncated = true
		return written, nil
	}
	if overflow := b.Len() + len(body) - b.limit; overflow > 0 {
		_ = b.Next(overflow)
		b.truncated = true
	}
	_, _ = b.Buffer.Write(body)
	return written, nil
}
