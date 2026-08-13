// Command codex-peer-lane manages durable Codex lanes.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if err := launcher.RunLane("lane", os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "codex-peer-lane: %v\n", err)
		var exit *launcher.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
