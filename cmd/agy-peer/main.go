// Command agy-peer launches owner-attested interactive Antigravity peers.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if err := launcher.RunAgyPeer(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agy-peer: %v\n", err)
		var exit *launcher.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
