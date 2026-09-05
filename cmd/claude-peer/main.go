package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/claude"
	"github.com/antst/agent-sessions/wrappers/host"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	if !host.LaneMode() {
		if len(arguments) == 1 && arguments[0] == "mcp" {
			return errors.New("mcp entry not available in this build")
		}
		return errors.New("interactive entry not available in this build")
	}
	if len(arguments) != 0 {
		return errors.New("lane mode accepts no arguments")
	}
	product := claude.New(os.Getenv(host.SocketEnv))
	worker := sessionkit.NewWorker(product)
	product.SetShutdown(worker.Shutdown)
	return worker.Serve(ctx)
}
