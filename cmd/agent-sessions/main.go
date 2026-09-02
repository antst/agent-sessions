// Command agent-sessions provides the one host daemon image and its short-lived multi-call clients.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/clihelp"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/servicecontrol"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

var version = "dev"

var currentServiceManager = servicecontrol.Current
var waitForServiceReady = waitForDaemonReady

type commandRunner func(context.Context, clihelp.Invocation, io.Writer) error

type commandRunners struct {
	daemon, status, doctor, roster, catalog commandRunner
	peer, lane, hook, connector             commandRunner
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[0], os.Args[1:], os.Stdout, defaultCommandRunners())
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
}

func run(ctx context.Context, argv0 string, args []string, output io.Writer, runners commandRunners) error {
	invocation, err := clihelp.Parse(argv0, args)
	if err != nil {
		return err
	}
	if invocation.Command == "help" {
		_, err := io.WriteString(output, clihelp.Usage())
		return err
	}
	if invocation.Command == "version" {
		_, err := fmt.Fprintln(output, version)
		return err
	}
	runner := map[string]commandRunner{
		"daemon": runners.daemon, "status": runners.status, "doctor": runners.doctor, "roster": runners.roster,
		"catalog": runners.catalog,
		"peer":    runners.peer, "lane": runners.lane, "hook": runners.hook, "connector": runners.connector,
	}[invocation.Command]
	if runner == nil {
		return fmt.Errorf("%s command runner is unavailable", invocation.Command)
	}
	return runner(ctx, invocation, output)
}

func defaultCommandRunners() commandRunners {
	return commandRunners{
		daemon: runDaemon,
		status: func(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
			return runAdmin(ctx, invocation, output, "status")
		},
		doctor: func(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
			return runAdmin(ctx, invocation, output, "doctor")
		},
		roster:  runRoster,
		catalog: runCatalog,
		peer: func(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
			if invocation.Product == "codex" {
				return launcher.RunCodexPeerWithDaemon(ctx, invocation.Arguments, requestCodexPreparation, runCodexNativePeer)
			}
			if invocation.Product == "claude" {
				return launcher.RunClaudePeer(invocation.Arguments)
			}
			if invocation.Product == "grok" {
				return launcher.RunGrokPeer(ctx, invocation.Arguments, runGrokNativePeer)
			}
			if invocation.Product == "qwen" {
				return launcher.RunQwenPeer(ctx, invocation.Arguments)
			}
			return launcher.RunManagedPeer(invocation.Product, invocation.Arguments)
		},
		lane: runLaneWorkflow,
		hook: func(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
			return runHook(ctx, invocation.Product, output)
		},
		connector: func(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
			return runConnector(ctx, invocation.Product, output)
		},
	}
}

//nolint:gocyclo // The thin multicall boundary validates help, stdin, daemon response, and doctor readiness in one place.
func runLaneWorkflow(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
	if len(invocation.Arguments) == 1 &&
		(invocation.Arguments[0] == "--help" || invocation.Arguments[0] == "-h" || invocation.Arguments[0] == "help") {
		usage, err := sessiontools.LaneUsage(invocation.Product)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(output, usage)
		return err
	}
	host, arguments, err := extractLaneHost(invocation.Arguments)
	if err != nil {
		return err
	}
	input, err := readLaneInput(ctx, arguments, os.Stdin)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve lane working directory: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"product": invocation.Product, "arguments": arguments, "host": host,
		"input": string(input), "cwd": cwd,
		"source_attachment_id": strings.TrimSpace(os.Getenv("AGENT_SESSIONS_SESSION_ID")),
	})
	if err != nil {
		return err
	}
	requestID := commandRequestID()
	response, err := callExistingDaemon(ctx, defaultStateRoot(), daemonpkg.ControlRequest{
		ID: requestID, Role: daemonpkg.RoleLauncher, Operation: "lane.command",
		IdempotencyKey: requestID, Payload: payload,
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return errors.New(response.Error.Message)
	}
	if err := writeJSONLine(output, response.Payload); err != nil {
		return err
	}
	if len(arguments) > 0 && arguments[0] == "doctor" {
		var report map[string]any
		if json.Unmarshal(response.Payload, &report) == nil {
			if ready, _ := report["ready"].(bool); !ready {
				return fmt.Errorf("%s lane readiness is not established: %v", invocation.Product, report["readiness_error"])
			}
		}
	}
	return nil
}

const maxLaneInputBytes = 1 << 20

type laneInputRead struct {
	body []byte
	err  error
}

func readLaneInput(ctx context.Context, arguments []string, input io.Reader) ([]byte, error) {
	if !laneCommandConsumesInput(arguments) {
		return nil, nil
	}
	completed := make(chan laneInputRead, 1)
	go func() {
		body, err := io.ReadAll(io.LimitReader(input, maxLaneInputBytes+1))
		completed <- laneInputRead{body: body, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-completed:
		if result.err != nil {
			return nil, fmt.Errorf("read lane input: %w", result.err)
		}
		if len(result.body) > maxLaneInputBytes {
			return nil, errors.New("lane input exceeds 1048576 bytes")
		}
		return result.body, nil
	}
}

func laneCommandConsumesInput(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	switch arguments[0] {
	case "run", "start", "resume":
		return true
	default:
		return false
	}
}

func extractLaneHost(arguments []string) (string, []string, error) {
	result := make([]string, 0, len(arguments))
	host := ""
	remaining := append([]string(nil), arguments...)
	for len(remaining) > 0 {
		argument := remaining[0]
		remaining = remaining[1:]
		if strings.HasPrefix(argument, "--host=") {
			if host != "" {
				return "", nil, errors.New("--host may be specified only once")
			}
			host = strings.TrimSpace(strings.TrimPrefix(argument, "--host="))
			if host == "" {
				return "", nil, errors.New("--host requires a value")
			}
			continue
		}
		if argument == "--host" {
			if host != "" {
				return "", nil, errors.New("--host may be specified only once")
			}
			if len(remaining) == 0 {
				return "", nil, errors.New("--host requires one value")
			}
			host = strings.TrimSpace(remaining[0])
			remaining = remaining[1:]
			if host == "" {
				return "", nil, errors.New("--host requires one value")
			}
			continue
		}
		result = append(result, argument)
	}
	return host, result, nil
}

func runDaemon(ctx context.Context, invocation clihelp.Invocation, output io.Writer) error {
	args := append([]string(nil), invocation.Arguments...)
	if isDaemonServiceAction(args) {
		return runDaemonServiceAction(ctx, args, output)
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	set := flag.NewFlagSet("agent-sessions daemon", flag.ContinueOnError)
	stateRoot := set.String("state-root", defaultStateRoot(), "durable Agent Sessions state root")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("agent-sessions daemon accepts run, start, stop, restart, and run service options")
	}
	coordinator := newHostCoordinator(ctx, *stateRoot)
	var runtime *daemonpkg.Runtime
	runtime, err := daemonpkg.StartRuntime(ctx, daemonpkg.RuntimeConfig{
		StateRoot: *stateRoot,
		Release:   version,
		Adapters:  coordinator.adapters(),
		Handler: func(callCtx context.Context, request daemonpkg.ControlRequest) (json.RawMessage, error) {
			return coordinator.handle(callCtx, runtime, request)
		},
		Components: []daemonpkg.RuntimeComponent{coordinator.run},
	})
	if err != nil {
		return err
	}
	coordinator.publishRuntime(runtime)
	return runtime.Wait()
}

func isDaemonServiceAction(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "start", "stop", "restart":
		return true
	default:
		return false
	}
}

func runDaemonServiceAction(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("agent-sessions daemon %s accepts no additional arguments", args[0])
	}
	manager, err := currentServiceManager()
	if err != nil {
		return err
	}
	action := args[0]
	switch action {
	case "start":
		err = manager.Start(ctx)
	case "stop":
		err = manager.Stop(ctx)
	case "restart":
		err = manager.Restart(ctx)
	}
	if err != nil {
		return err
	}
	if action != "stop" {
		if err := waitForServiceReady(ctx, defaultStateRoot()); err != nil {
			return fmt.Errorf("agent sessions service did not become ready after %s: %w", action, err)
		}
	}
	return writeJSONLine(output, json.RawMessage(fmt.Sprintf(`{"service":"agent-sessions","action":%q,"completed":true}`, action)))
}

func waitForDaemonReady(parent context.Context, stateRoot string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		response, err := callExistingDaemon(ctx, stateRoot, daemonpkg.ControlRequest{
			ID: commandRequestID(), Role: daemonpkg.RoleAdmin, Operation: "status",
		})
		if err == nil && response.Error == nil {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New(response.Error.Message)
		}
		select {
		case <-ctx.Done():
			return errors.Join(lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func runAdmin(ctx context.Context, invocation clihelp.Invocation, output io.Writer, operation string) error {
	stateRoot, err := parseStateRoot(invocation.Command, invocation.Arguments)
	if err != nil {
		return err
	}
	response, err := callExistingDaemon(ctx, stateRoot, daemonpkg.ControlRequest{
		ID: commandRequestID(), Role: daemonpkg.RoleAdmin, Operation: operation,
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return errors.New(response.Error.Message)
	}
	return writeJSONLine(output, response.Payload)
}

func runWorkflow(ctx context.Context, invocation clihelp.Invocation, output io.Writer, role daemonpkg.ControlRole, operation string) error {
	payload, err := json.Marshal(map[string]any{"product": invocation.Product, "arguments": invocation.Arguments})
	if err != nil {
		return err
	}
	requestID := commandRequestID()
	response, err := callExistingDaemon(ctx, defaultStateRoot(), daemonpkg.ControlRequest{
		ID: requestID, Role: role, Operation: operation, IdempotencyKey: requestID, Payload: json.RawMessage(payload),
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		if role == daemonpkg.RoleHook && response.Error.Code == daemonpkg.ErrorInactive {
			return nil
		}
		return errors.New(response.Error.Message)
	}
	if len(response.Payload) == 0 {
		return nil
	}
	return writeJSONLine(output, response.Payload)
}

func callExistingDaemon(ctx context.Context, stateRoot string, request daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error) {
	endpoint, err := daemonpkg.ControlEndpoint(stateRoot)
	if err != nil {
		return daemonpkg.ControlResponse{}, err
	}
	return callDaemonWithStableRetry(ctx, endpoint, request, daemonpkg.CallControl)
}

const (
	maxDaemonControlAttempts = 3
	daemonControlRetryDelay  = 10 * time.Millisecond
)

type daemonControlCaller func(context.Context, string, daemonpkg.ControlRequest) (daemonpkg.ControlResponse, error)

func callDaemonWithStableRetry(
	ctx context.Context,
	endpoint string,
	request daemonpkg.ControlRequest,
	call daemonControlCaller,
) (daemonpkg.ControlResponse, error) {
	request.Generation = 0
	for attempt := 0; attempt < maxDaemonControlAttempts; attempt++ {
		response, err := call(ctx, endpoint, request)
		if err != nil {
			if attempt+1 == maxDaemonControlAttempts {
				return daemonpkg.ControlResponse{}, fmt.Errorf("agent sessions daemon is unavailable: %w", err)
			}
			if err := waitDaemonControlRetry(ctx); err != nil {
				return daemonpkg.ControlResponse{}, fmt.Errorf("agent sessions daemon is unavailable: %w", err)
			}
			continue
		}
		if response.Error == nil || response.Error.Code != daemonpkg.ErrorStaleGeneration ||
			response.Generation == 0 || response.Generation == request.Generation {
			return response, nil
		}
		if attempt+1 == maxDaemonControlAttempts {
			return response, nil
		}
		request.Generation = response.Generation
	}
	return daemonpkg.ControlResponse{}, errors.New("agent sessions daemon retry loop ended without a response")
}

func waitDaemonControlRetry(ctx context.Context) error {
	timer := time.NewTimer(daemonControlRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseStateRoot(command string, args []string) (string, error) {
	set := flag.NewFlagSet("agent-sessions "+command, flag.ContinueOnError)
	stateRoot := set.String("state-root", defaultStateRoot(), "durable Agent Sessions state root")
	if err := set.Parse(args); err != nil {
		return "", err
	}
	if set.NArg() != 0 {
		return "", fmt.Errorf("agent-sessions %s received unexpected arguments", command)
	}
	return *stateRoot, nil
}

func defaultStateRoot() string {
	if configured := os.Getenv("AGENT_SESSIONS_STATE_ROOT"); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agent-sessions-state-unavailable")
	}
	return filepath.Join(home, ".local", "state", "agent-sessions")
}

func commandRequestID() string {
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return fmt.Sprintf("request-%d", os.Getpid())
	}
	return hex.EncodeToString(body)
}

func writeJSONLine(output io.Writer, payload json.RawMessage) error {
	if !json.Valid(payload) {
		return errors.New("daemon returned invalid JSON")
	}
	_, err := fmt.Fprintln(output, string(payload))
	return err
}
