// Command claude-peer-lane manages durable Claude Code lanes.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if err := launcher.RunLane("claude-lane", os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "claude-peer-lane: %v\n", err)
		var exit *launcher.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
