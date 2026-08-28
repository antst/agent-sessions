// Command agent-sessions-hub runs the central Agent Sessions federation hub.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/antst/agent-sessions/internal/clihelp"
	"github.com/antst/agent-sessions/internal/federation"
)

var version = "development"

// acceptanceResourceFailure is populated only by isolated installed-service
// acceptance builds. Release builds leave it empty and use real resource
// preflight. Keeping the selector linker-only avoids an operator-facing fault
// injection environment variable in the production service.
var acceptanceResourceFailure string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

//nolint:gocyclo // One closed descriptor-backed switch keeps every hub authority visible.
func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	command, remainder, err := clihelp.ResolveCommand("agent-sessions-hub", args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-sessions-hub: %v\n", err)
		return 2
	}
	if hasHelp(args) {
		body, renderErr := clihelp.RenderHelp(command.Key)
		if renderErr != nil {
			_, _ = fmt.Fprintf(stderr, "agent-sessions-hub: %v\n", renderErr)
			return 2
		}
		_, _ = io.WriteString(stdout, body)
		return 0
	}
	parsed, err := clihelp.ParseOptions(command.Key, remainder)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-sessions-hub: %v\n", err)
		return 2
	}
	var output []byte
	switch command.Key {
	case "hub.serve":
		listen, parseErr := hubListenArgument(remainder)
		if parseErr != nil {
			err = parseErr
			break
		}
		identity, identityErr := federation.CurrentHubRuntimeIdentity()
		if identityErr != nil {
			err = identityErr
			break
		}
		err = federation.RunHub(ctx, federation.HubOptions{
			Listen: listen, RuntimeVersion: version, RuntimeIdentity: identity,
			ServiceManager: hubServiceManager(), ServiceUnit: hubServiceUnit(),
			ResourcePreflight: acceptanceHubResourcePreflight(acceptanceResourceFailure),
			Stdout:            stdout, Stderr: stderr,
		})
	case "hub.status":
		var status federation.HubStatusProjection
		status, err = federation.ReadHubStatus(ctx)
		if err == nil {
			output, err = federation.RenderHubStatus(status, parsed.JSON)
		}
	case "hub.doctor":
		output, err = federation.RenderHubDoctor(federation.InspectHubDoctor(ctx), parsed.JSON)
	case "hub.remove.inspect":
		var inspection federation.HubRemovalInspection
		inspection, err = federation.InspectHubRemoval(defaultHubPrefix())
		if err == nil {
			output, err = renderHubCLIValue("hub.remove.inspect", inspection, parsed.JSON)
		}
	case "hub.purge.inspect":
		var inspection federation.HubPurgeInspection
		inspection, err = federation.RunHubPurgeInspectCLI(ctx, defaultHubPrefix(), parsed.Plan)
		if err == nil {
			output, err = renderHubCLIValue("hub.purge.inspect", inspection, parsed.JSON)
		}
	case "hub.purge.apply":
		var result federation.HubPurgeApplyResult
		result, err = federation.RunHubPurgeApplyCLI(ctx, defaultHubPrefix(), parsed.Plan)
		if err == nil {
			output, err = renderHubCLIValue("hub.purge.apply", result, parsed.JSON)
		}
	case "hub.install":
		err = federation.RunHubInstallCLI(ctx, remainder)
	case "hub.remove.apply":
		err = federation.RunHubRemoveCLI(ctx, remainder)
	default:
		err = fmt.Errorf("unsupported hub command %s", command.Key)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-sessions-hub: %v\n", err)
		return hubExitCode(err)
	}
	if len(output) != 0 {
		_, _ = stdout.Write(append(output, '\n'))
	}
	return 0
}

func acceptanceHubResourcePreflight(resource string) func(context.Context, federation.HubAdmissionRequest) error {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return nil
	}
	causes := map[string]error{
		"disk": syscall.ENOSPC, "memory": syscall.ENOMEM,
		"file_descriptor": syscall.EMFILE, "process": syscall.EAGAIN,
	}
	cause, ok := causes[resource]
	if !ok {
		return func(context.Context, federation.HubAdmissionRequest) error {
			return fmt.Errorf("unknown acceptance resource failure %q", resource)
		}
	}
	return func(context.Context, federation.HubAdmissionRequest) error {
		return fmt.Errorf("%s resource unavailable: %w", resource, cause)
	}
}

func hasHelp(args []string) bool {
	for _, argument := range args {
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}

func hubListenArgument(arguments []string) (string, error) {
	listen := ":7419"
	for len(arguments) != 0 {
		if len(arguments) != 2 || arguments[0] != "--listen" || arguments[1] == "" {
			return "", errors.New("usage: agent-sessions-hub [--listen ADDRESS]")
		}
		listen, arguments = arguments[1], arguments[2:]
	}
	return listen, nil
}

func hubServiceManager() string {
	if runtime.GOOS == "darwin" {
		return "launchd"
	}
	return "systemd-user"
}

func hubServiceUnit() string {
	if runtime.GOOS == "darwin" {
		return "net.antst.agent-sessions-hub"
	}
	return "agent-sessions-hub.service"
}

func defaultHubPrefix() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local")
}

func renderHubCLIValue(event string, value any, machine bool) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if machine {
		return body, nil
	}
	return []byte(event + " " + string(body)), nil
}

func hubExitCode(err error) int {
	var classified interface{ ExitCode() int }
	if errors.As(err, &classified) {
		return classified.ExitCode()
	}
	if os.IsNotExist(err) {
		return 3
	}
	return 1
}
