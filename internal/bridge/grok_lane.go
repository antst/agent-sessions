package bridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const grokLaneContractVersion = 1

type grokLaneOptions struct {
	command string
	target  string
	help    bool
}

func grokLaneUsage() string {
	return `grok-peer-lane — named, messageable Grok Build lanes

Usage:
  grok-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane resume SESSION_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane wait SESSION_OR_NAME [--timeout SECONDS]
  grok-peer-lane status SESSION_OR_NAME
  grok-peer-lane interrupt SESSION_OR_NAME
  grok-peer-lane archive SESSION_OR_NAME
  grok-peer-lane list [--all] [--mine]
  grok-peer-lane doctor [--json]

The lane owns a headless Grok ACP session. It never opens or concurrently
writes an interactive grok-peer conversation.
`
}

func parseGrokLaneArgs(argv []string) (grokLaneOptions, error) {
	if len(argv) == 0 {
		return grokLaneOptions{help: true}, nil
	}
	for _, argument := range argv {
		if argument == "-h" || argument == "--help" {
			return grokLaneOptions{help: true}, nil
		}
	}
	command := argv[0]
	if !containsString([]string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"}, command) {
		return grokLaneOptions{}, fmt.Errorf("unknown command %q", command)
	}
	options := grokLaneOptions{command: command}
	if containsString([]string{"resume", "wait", "status", "interrupt", "archive"}, command) {
		if len(argv) < 2 || strings.TrimSpace(argv[1]) == "" || strings.HasPrefix(argv[1], "-") {
			return grokLaneOptions{}, fmt.Errorf("%s requires a session ID or lane name", command)
		}
		options.target = argv[1]
	}
	return options, nil
}

func runGrokLaneCommand(argv []string) int {
	options, err := parseGrokLaneArgs(argv)
	if err != nil {
		_ = emitLane(map[string]any{"type": "error", "message": err.Error()})
		fmt.Fprintf(os.Stderr, "grok-peer-lane: %v\n", err)
		return 1
	}
	if options.help {
		fmt.Print(grokLaneUsage())
		return 0
	}
	if options.command == "doctor" {
		code, doctorErr := doctorGrokLane()
		if doctorErr != nil {
			_ = emitLane(map[string]any{"type": "error", "message": doctorErr.Error()})
			fmt.Fprintf(os.Stderr, "grok-peer-lane: %v\n", doctorErr)
			return 1
		}
		return code
	}
	err = fmt.Errorf("%s is not implemented yet on this draft branch", options.command)
	_ = emitLane(map[string]any{"type": "error", "message": err.Error()})
	fmt.Fprintf(os.Stderr, "grok-peer-lane: %v\n", err)
	return 1
}

func doctorGrokLane() (int, error) {
	paths := resolveNativePaths()
	grokBin := strings.TrimSpace(os.Getenv("GROK_PEER_GROK_BIN"))
	available := grokBin != ""
	version := ""
	versionError := ""
	if available {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, grokBin, "--no-auto-update", "--version") // #nosec G204 -- launcher supplies a fail-closed validated Grok Build executable.
		body, err := command.Output()
		cancel()
		switch {
		case ctx.Err() != nil:
			versionError = "Grok Build version check timed out"
		case err != nil:
			versionError = err.Error()
		default:
			version = strings.TrimSpace(string(body))
		}
	}
	_, supervisorErr := requestControl(paths.supervisorSock, map[string]any{"action": "status"}, 2*time.Second)
	supervisorReachable := supervisorErr == nil
	executable, _ := os.Executable()
	if err := emitLane(map[string]any{
		"type": "lane.doctor", "product": "grok", "contract_version": grokLaneContractVersion,
		"runtime_path": executable, "grok_available": available, "grok_path": emptyStringAsNil(grokBin),
		"grok_version": emptyStringAsNil(version), "grok_error": emptyStringAsNil(versionError),
		"state_root": profileDataRoot(paths), "supervisor_reachable": supervisorReachable,
		"supervisor_socket": paths.supervisorSock,
	}); err != nil {
		return 1, err
	}
	if !available || versionError != "" || !supervisorReachable {
		return 1, nil
	}
	return 0, nil
}
