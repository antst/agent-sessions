// Command agent-sessions is the canonical multi-call host executable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/antst/agent-sessions/internal/bridge"
	"github.com/antst/agent-sessions/internal/clihelp"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	code := run(filepathBase(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func run(argv0 string, args []string, stdout, stderr io.Writer) int {
	command, remainder, err := clihelp.ResolveCommand(argv0, args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", filepathBase(argv0), err)
		return 2
	}
	if requestsHelp(args, remainder) || command.Key == "host.help" {
		return renderCommandHelp(command, remainder, stdout, stderr)
	}
	if command.DaemonRole == "admin" {
		options, parseErr := clihelp.ParseOptions(command.Key, remainder)
		if parseErr != nil || len(options.Arguments) != 0 {
			if parseErr == nil {
				parseErr = fmt.Errorf("unexpected positional arguments %q", options.Arguments)
			}
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", filepathBase(argv0), parseErr)
			return 2
		}
		return runAdministrativeCommand(command, options, stdout, stderr)
	}

	err = dispatchHostCommand(command, remainder)
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "%s: %v\n", filepathBase(argv0), err)
	var launcherExit *launcher.ExitError
	if errors.As(err, &launcherExit) {
		return launcherExit.Code
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}

func runAdministrativeCommand(command clihelp.CommandDescriptor, options clihelp.ParsedOptions, stdout, stderr io.Writer) int {
	if command.Key == "host.purge.inspect" {
		result, err := daemon.RunHostPurgeInspectCLI(context.Background(), options.Plan)
		if err != nil {
			return renderAdministrativeFailure(command, options.JSON, err, stdout, stderr)
		}
		return renderAdministrativeResult(command, options.JSON, result, stdout, stderr)
	}
	if command.Key == "host.purge.apply" {
		result, err := daemon.RunHostPurgeApplyCLI(context.Background(), options.Plan)
		if err != nil {
			return renderAdministrativeFailure(command, options.JSON, err, stdout, stderr)
		}
		return renderAdministrativeResult(command, options.JSON, result, stdout, stderr)
	}
	operation := map[string]string{
		"host.status": "runtime.status", "host.doctor": "runtime.doctor",
		"host.migrate.inspect": "migration.inspect", "host.remove.inspect": "remove.inspect",
	}[command.Key]
	if operation == "" {
		return renderAdministrativeFailure(command, options.JSON, &commandUnavailableError{command: command.Key}, stdout, stderr)
	}
	result, err := daemon.QueryAdmin(context.Background(), operation)
	if err != nil {
		return renderAdministrativeFailure(command, options.JSON, err, stdout, stderr)
	}
	if !options.JSON {
		_, _ = fmt.Fprintln(stdout, string(result))
		return 0
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		return renderAdministrativeFailure(command, true, fmt.Errorf("decode daemon result: %w", err), stdout, stderr)
	}
	writeJSON(stdout, map[string]any{
		"schema_version": 1, "ok": true, "command": command.JSONResultContract, "result": decoded,
	})
	return 0
}

func renderAdministrativeResult(command clihelp.CommandDescriptor, machine bool, result any, stdout, stderr io.Writer) int {
	body, err := json.Marshal(result)
	if err != nil {
		return renderAdministrativeFailure(command, machine, err, stdout, stderr)
	}
	if !machine {
		_, _ = fmt.Fprintln(stdout, string(body))
		return 0
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return renderAdministrativeFailure(command, true, err, stdout, stderr)
	}
	writeJSON(stdout, map[string]any{
		"schema_version": 1, "ok": true, "command": command.JSONResultContract, "result": decoded,
	})
	return 0
}

func renderAdministrativeFailure(command clihelp.CommandDescriptor, machine bool, err error, stdout, stderr io.Writer) int {
	code, class, errorCode, retryable, nextAction := 1, "internal", "internal", false, "inspect Agent Sessions service logs"
	var unavailable *daemon.UnavailableError
	if errors.As(err, &unavailable) {
		code, class, errorCode, retryable, nextAction = 3, "unavailable", "daemon_unavailable", true, unavailable.NextAction
	} else {
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			code = coded.ExitCode()
			class, errorCode, retryable, nextAction = mapExitFailure(code)
		}
	}
	if !machine {
		_, _ = fmt.Fprintf(stderr, "agent-sessions: %v\n", err)
		return code
	}
	message := err.Error()
	if class == "unavailable" {
		message = "agent-sessions daemon is unavailable"
	}
	writeJSON(stdout, map[string]any{
		"schema_version": 1, "ok": false, "command": command.JSONResultContract,
		"error": map[string]any{"class": class, "code": errorCode, "retryable": retryable, "message": message, "next_action": nextAction},
	})
	return code
}

func mapExitFailure(code int) (class, errorCode string, retryable bool, nextAction string) {
	switch code {
	case 3:
		return "unavailable", "daemon_unavailable", true, daemon.InspectionCommand()
	case 4:
		return "refused", "unsafe_precondition", false, "resolve every exact blocker and retry"
	case 5:
		return "incompatible", "incompatible_contract", false, "install a compatible Agent Sessions release"
	case 6:
		return "retryable", "lifecycle_debt", true, "inspect lifecycle debt and retry the exact operation"
	default:
		return "internal", "internal", false, "inspect Agent Sessions service logs"
	}
}

func writeJSON(writer io.Writer, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(writer, string(body))
}

func dispatchHostCommand(command clihelp.CommandDescriptor, args []string) error {
	switch command.Key {
	case "host.daemon":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		codex := bridge.NewCodexDaemonAdapter()
		defer codex.Close()
		claude := bridge.NewClaudeDaemonAdapter()
		defer claude.Close()
		grok := bridge.NewGrokDaemonAdapter()
		defer grok.Close()
		qwen := bridge.NewQwenDaemonAdapter()
		defer qwen.Close()
		return daemon.RunForegroundWithOptions(ctx, daemon.ForegroundOptions{
			AttachmentAdapters: map[string]daemon.AttachmentAdapter{"codex": codex, "claude": claude, "grok": grok, "qwen": qwen},
			DeliveryAdapters:   map[string]daemon.DeliveryAdapter{"codex": codex, "claude": claude, "grok": grok, "qwen": qwen},
		})
	case "host.connector.install":
		return daemon.RunConnectorLifecycleCLI(context.Background(), "install", args)
	case "host.connector.remove":
		return daemon.RunConnectorLifecycleCLI(context.Background(), "remove", args)
	case "host.install":
		return daemon.RunHostInstallCLI(context.Background(), args)
	case "host.remove.apply":
		return daemon.RunHostRemoveCLI(context.Background(), args)
	case "peer":
		return launcher.RunPeer(args)
	case "peer.codex":
		return launcher.RunCodexPeer(args)
	case "peer.claude":
		return launcher.RunClaudePeer(args)
	case "peer.grok":
		return launcher.RunGrokPeer(args)
	case "peer.qwen":
		return launcher.RunQwenPeer(args)
	}
	if strings.HasPrefix(command.Key, "lane.") {
		parts := strings.Split(command.Key, ".")
		if len(parts) != 3 {
			return fmt.Errorf("invalid lane command identity %q", command.Key)
		}
		role := map[string]string{"codex": "lane", "claude": "claude-lane", "grok": "grok-lane", "qwen": "qwen-lane"}[parts[1]]
		return launcher.RunLane(role, append([]string{parts[2]}, args...))
	}
	// These canonical routes are deliberately present before their later story
	// implementations. They do not start a daemon or fall back to a legacy
	// executable.
	return &commandUnavailableError{command: command.Key}
}

type commandUnavailableError struct{ command string }

func (failure *commandUnavailableError) Error() string {
	return fmt.Sprintf("%s is unavailable until its daemon component is active", failure.command)
}
func (*commandUnavailableError) ExitCode() int { return 3 }

func renderCommandHelp(command clihelp.CommandDescriptor, remainder []string, stdout, stderr io.Writer) int {
	jsonMode := false
	for _, argument := range remainder {
		if argument == "--json" {
			jsonMode = true
		}
	}
	if command.Key == "host.help" && len(remainder) > 0 && remainder[0] != "--json" && remainder[0] != "--help" && remainder[0] != "-h" {
		if selected, ok := clihelp.CommandByKey(remainder[0]); ok {
			command = selected
		} else {
			_, _ = fmt.Fprintf(stderr, "agent-sessions: unknown help mode %q\n", remainder[0])
			return 2
		}
	}
	if jsonMode && command.Key == "host.help" {
		contract := clihelp.Contract()
		body, err := json.Marshal(map[string]any{
			"schema_version": 1, "ok": true, "command": "help",
			"result": map[string]any{"binaries": contract.ReleaseExecutables, "modes": contract.Commands},
		})
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "agent-sessions: render JSON help: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, string(body))
		return 0
	}
	body, err := clihelp.RenderHelp(command.Key)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-sessions: %v\n", err)
		return 2
	}
	_, _ = io.WriteString(stdout, body)
	return 0
}

func requestsHelp(original, remainder []string) bool {
	for _, arguments := range [][]string{original, remainder} {
		for _, argument := range arguments {
			if argument == "--help" || argument == "-h" {
				return true
			}
		}
	}
	return false
}

func filepathBase(path string) string {
	if index := strings.LastIndexAny(path, `/\\`); index >= 0 {
		return path[index+1:]
	}
	return path
}
