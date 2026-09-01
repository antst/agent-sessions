// Command agent-sessions-hub runs the independently deployed central hub.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/antst/agent-sessions/internal/federation"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-sessions-hub: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	set := flag.NewFlagSet("agent-sessions-hub", flag.ExitOnError)
	listen := set.String("listen", envOr("AGENT_SESSIONS_HUB_LISTEN", ":7419"), "TCP address to listen on")
	showVersion := set.Bool("version", false, "print the build version")
	_ = set.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	federation.RuntimeVersion = version
	return federation.RunHub(ctx, federation.HubOptions{Listen: *listen})
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
