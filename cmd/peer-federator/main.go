// Command peer-federator runs the federation hub, host agent, and diagnostics.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/antst/agent-sessions/internal/federator"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if message := err.Error(); message != "" {
			fmt.Fprintf(os.Stderr, "peer-federator: %s\n", message)
		}
		exitCode := 1
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) && coded.ExitCode() > 0 {
			exitCode = coded.ExitCode()
		}
		os.Exit(exitCode)
	}
}

type commandExitError struct {
	code int
	err  error
}

func (e commandExitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}
func (e commandExitError) Unwrap() error { return e.err }
func (e commandExitError) ExitCode() int { return e.code }

func run(args []string) error {
	federator.RuntimeVersion = version
	if len(args) == 0 {
		return errors.New(usage())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "hub":
		return runHub(ctx, args[1:])
	case "agent":
		return runAgent(ctx, args[1:])
	case "doctor":
		return runDoctor(ctx, args[1:])
	case "status":
		return runStatus(args[1:])
	case "hosts":
		return runHosts(args[1:])
	case "lane":
		return runLane(ctx, args[1:])
	case "lane-watch":
		return runLaneWatch(ctx, args[1:])
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage())
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage())
	}
}

func runHub(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("hub", flag.ContinueOnError)
	listen := set.String("listen", envDefault("PEER_FEDERATOR_LISTEN", ":7419"), "TCP address to listen on")
	if err := set.Parse(args); err != nil {
		return err
	}
	return federator.RunHub(ctx, federator.HubOptions{Listen: *listen})
}

func runAgent(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("agent", flag.ContinueOnError)
	hub := set.String("hub", os.Getenv("PEER_FEDERATOR_HUB"), "optional hub host:port")
	host := set.String("host", envDefault("PEER_FEDERATOR_HOST", defaultHostID()), "stable simple host identifier")
	name := set.String("name", os.Getenv("PEER_FEDERATOR_NAME"), "human-readable host name (defaults to --host)")
	claudeConfig := set.String("claude-config-dir", federator.DefaultClaudeConfigDir(), "Claude configuration directory")
	runtimeDir := set.String("runtime-dir", federator.DefaultRuntimeDir(), "ephemeral runtime directory")
	stateDir := set.String("state-dir", "", "durable grouped-session catalog directory")
	enableRemoteLanes := set.Bool("enable-remote-lanes", envEnabled("PEER_FEDERATOR_ENABLE_REMOTE_LANES"), "allow hub peers to execute native lane commands on this host")
	codexLane := set.String("codex-lane", os.Getenv("PEER_FEDERATOR_CODEX_LANE"), "codex-peer-lane executable")
	claudeLane := set.String("claude-lane", os.Getenv("PEER_FEDERATOR_CLAUDE_LANE"), "claude-peer-lane executable")
	grokLane := set.String("grok-lane", os.Getenv("PEER_FEDERATOR_GROK_LANE"), "grok-peer-lane executable")
	qwenLane := set.String("qwen-lane", os.Getenv("PEER_FEDERATOR_QWEN_LANE"), "qwen-peer-lane executable")
	if err := set.Parse(args); err != nil {
		return err
	}
	return federator.RunAgent(ctx, federator.AgentOptions{
		Hub: *hub, HostID: *host, HostName: *name,
		ClaudeConfigDir: *claudeConfig, RuntimeDir: *runtimeDir, StateDir: *stateDir,
		EnableRemoteLanes:   *enableRemoteLanes,
		CodexLaneExecutable: *codexLane, ClaudeLaneExecutable: *claudeLane,
		GrokLaneExecutable: *grokLane, QwenLaneExecutable: *qwenLane,
	})
}

func defaultHostID() string {
	host, _ := os.Hostname()
	if host == "" {
		return "local"
	}
	return host
}

func runDoctor(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	hub := set.String("hub", os.Getenv("PEER_FEDERATOR_HUB"), "hub host:port")
	claudeConfig := set.String("claude-config-dir", federator.DefaultClaudeConfigDir(), "Claude configuration directory")
	runtimeDir := set.String("runtime-dir", federator.DefaultRuntimeDir(), "ephemeral runtime directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	report := federator.Doctor(ctx, federator.DoctorOptions{
		Hub: *hub, ClaudeConfigDir: *claudeConfig, RuntimeDir: *runtimeDir,
	})
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return err
	}
	if !report.OK {
		return errors.New(report.Summary)
	}
	return nil
}

func runStatus(args []string) error {
	set := flag.NewFlagSet("status", flag.ContinueOnError)
	runtimeDir := set.String("runtime-dir", federator.DefaultRuntimeDir(), "ephemeral runtime directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	status, err := federator.ReadAgentStatus(*runtimeDir)
	if err != nil {
		return fmt.Errorf("agent is not reachable: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}

func runHosts(args []string) error {
	set := flag.NewFlagSet("hosts", flag.ContinueOnError)
	runtimeDir := set.String("runtime-dir", federator.DefaultRuntimeDir(), "ephemeral runtime directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	hosts, err := federator.ReadRemoteHosts(*runtimeDir)
	if err != nil {
		return fmt.Errorf("cannot list remote hosts: %w", err)
	}
	if hosts == nil {
		hosts = []federator.Host{}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type": "host.list", "protocol_version": federator.ProtocolVersion, "hosts": hosts,
	})
}

func runLane(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("lane", flag.ContinueOnError)
	host := set.String("host", "", "connected destination host id or unique name")
	product := set.String("product", "", "lane product: codex, claude, or grok")
	sourceSession := set.String("source-session", "", "originating live local peer session (normally inferred)")
	runtimeDir := set.String("runtime-dir", federator.DefaultRuntimeDir(), "ephemeral runtime directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	code, err := federator.RunRemoteLane(ctx, federator.RemoteLaneOptions{
		RuntimeDir: *runtimeDir, Host: *host, Product: *product,
		SourceSession: *sourceSession, Args: set.Args(),
	}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return commandExitError{code: code, err: err}
	}
	if code != 0 {
		return commandExitError{code: code}
	}
	return nil
}

func runLaneWatch(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("lane-watch", flag.ContinueOnError)
	livenessFD := set.Uint64("liveness-fd", 3, "inherited agent liveness descriptor")
	if err := set.Parse(args); err != nil {
		return err
	}
	code, err := federator.RunLaneWatch(ctx, uintptr(*livenessFD), set.Args())
	if err != nil {
		return err
	}
	if code != 0 {
		return commandExitError{code: code}
	}
	return nil
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func usage() string {
	return `peer-federator — route grouped Agent Sessions peers locally and across a trusted LAN

Usage:
  peer-federator hub   [--listen :7419]
  peer-federator agent --hub HOST:PORT --host HOST_ID [--name DISPLAY_NAME] [--enable-remote-lanes]
  peer-federator doctor --hub HOST:PORT
  peer-federator status
  peer-federator hosts
  peer-federator lane --host HOST --product codex|claude|grok -- COMMAND [ARGS...]
  peer-federator version

Remote lane execution is disabled by default. Run peer-federator COMMAND --help for all options.
`
}
