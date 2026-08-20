// Command grok-peer-lane manages durable Grok Build lanes.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if err := launcher.RunLane("grok-lane", os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "grok-peer-lane: %v\n", err)
		var exit *launcher.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
