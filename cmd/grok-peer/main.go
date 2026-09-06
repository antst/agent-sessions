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
	"github.com/antst/agent-sessions/wrappers/grok"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

func main() {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer func() { signal.Stop(signals); cancel(nil) }()
	go func() {
		select {
		case caught := <-signals:
			cancel(signalCause{caught})
		case <-ctx.Done():
		}
	}()
	if err := run(ctx, os.Args[1:]); err != nil {
		cancel(nil)
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			if status, ok := exited.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signal.Reset(status.Signal())
				_ = syscall.Kill(os.Getpid(), status.Signal())
			}
			os.Exit(exited.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type signalCause struct{ signal os.Signal }

func (s signalCause) Error() string           { return s.signal.String() }
func (s signalCause) CaughtSignal() os.Signal { return s.signal }

func run(ctx context.Context, arguments []string) error {
	if !host.LaneMode() {
		if len(arguments) == 1 && arguments[0] == "mcp" {
			return runMCP(ctx)
		}
		plan, err := grok.InteractivePlan(arguments, os.Environ())
		if err != nil {
			return err
		}
		return grok.RunInteractive(ctx, plan)
	}
	if len(arguments) != 0 {
		return errors.New("lane mode accepts no arguments")
	}
	product := grok.New(os.Getenv(host.SocketEnv), os.Getenv(host.TokenEnv))
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
	backend, err := grok.NewPeerBackend(ctx, os.Environ())
	if err != nil {
		return err
	}
	defer backend.Shutdown()
	return (&mcp.Server{Backend: backend}).Serve(ctx, os.Stdin, os.Stdout)
}
