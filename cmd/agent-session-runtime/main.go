// Command agent-session-runtime provides the shared session runtime.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/bridge"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "connector-install" || os.Args[1] == "connector-remove") {
		operation := map[string]string{"connector-install": "install", "connector-remove": "remove"}[os.Args[1]]
		if err := daemon.RunConnectorLifecycleCLI(context.Background(), operation, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "agent-session-runtime %s: %v\n", os.Args[1], err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if len(os.Args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: agent-session-runtime bootstrap")
			os.Exit(2)
		}
		if _, err := launcher.EnsureRuntime(); err != nil {
			fmt.Fprintf(os.Stderr, "agent-session-runtime bootstrap: %v\n", err)
			var exit *launcher.ExitError
			if errors.As(err, &exit) {
				os.Exit(exit.Code)
			}
			var coded interface{ ExitCode() int }
			if errors.As(err, &coded) {
				os.Exit(coded.ExitCode())
			}
			os.Exit(1)
		}
		return
	}
	bridge.Main()
}
