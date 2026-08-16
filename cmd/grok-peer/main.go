// Command grok-peer launches owner-attested interactive Grok peers.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if err := launcher.RunGrokPeer(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "grok-peer: %v\n", err)
		var exit *launcher.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
