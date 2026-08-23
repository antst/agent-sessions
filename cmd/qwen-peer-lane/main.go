// Command qwen-peer-lane manages durable Qwen Code lanes.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/launcher"
)

func main() {
	if err := launcher.RunLane("qwen-lane", os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "qwen-peer-lane: %v\n", err)
		var exit *launcher.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
