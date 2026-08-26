// Command qwen-peer launches native Qwen sessions that participate in Agent Sessions groups.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if err := launcher.RunQwenPeer(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "qwen-peer: %v\n", err)
		var exit *launcher.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
