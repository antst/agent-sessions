// Command agent-sessions-hub runs the central Agent Sessions federation hub.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/antst/agent-sessions/internal/clihelp"
)

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
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
	_ = remainder
	// Hub behavior is implemented only by this binary in the federation story;
	// the host image has no route that can become a central hub.
	_, _ = fmt.Fprintf(stderr, "agent-sessions-hub: %s is unavailable until its hub component is active\n", command.Key)
	return 3
}

func hasHelp(args []string) bool {
	for _, argument := range args {
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}
