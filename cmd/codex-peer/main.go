package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/codex"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
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
			return runMCP(ctx)
		}
		plan, err := codex.InteractivePlan(arguments, os.Environ())
		if err != nil {
			return err
		}
		path, err := exec.LookPath(plan.Path)
		if err != nil {
			return err
		}
		return syscall.Exec(path, append([]string{path}, plan.Args...), plan.Env)
	}
	if len(arguments) != 0 {
		return errors.New("lane mode accepts no arguments")
	}
	product := codex.New(os.Getenv(host.SocketEnv), host.LaunchTokenDigest(os.Getenv(host.TokenEnv)))
	worker := sessionkit.NewWorker(product)
	product.SetShutdown(worker.Shutdown)
	product.SetCall(func(ctx context.Context, method string, params any) (json.RawMessage, error) {
		var result json.RawMessage
		err := worker.Call(ctx, method, params, &result)
		return result, err
	})
	return worker.Serve(ctx)
}

func runMCP(ctx context.Context) error {
	if os.Getenv(mcp.LaneSocketEnv) != "" {
		backend, err := mcp.NewLaneBackend()
		if err != nil {
			return err
		}
		return (&mcp.Server{Backend: backend}).Serve(ctx, os.Stdin, os.Stdout)
	}
	backend, err := codex.NewPeerBackend(ctx)
	if err != nil {
		return err
	}
	defer backend.Shutdown()
	return (&mcp.Server{Backend: backend}).Serve(ctx, os.Stdin, os.Stdout)
}
