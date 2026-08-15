// Command agent-session-runtime provides the shared session runtime.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/bridge"
	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agy-plugin-manifest" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: agent-session-runtime agy-plugin-manifest PATH")
			os.Exit(2)
		}
		if err := launcher.RegisterAgyPluginManifest(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "agent-session-runtime agy-plugin-manifest: %v\n", err)
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
			os.Exit(1)
		}
		return
	}
	bridge.Main()
}
