// Command agent-sessions-hub runs the independently deployed central hub.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/antst/sessionbus/internal/federation"
)

var version = "dev"

const maxHubConfigBytes = 64 * 1024

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-sessions-hub: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithContext(ctx, os.Args[1:], home)
}

func runWithContext(ctx context.Context, arguments []string, home string) error {
	configuredListen, err := hubListenAddress(os.Getenv, home)
	if err != nil {
		return err
	}
	set := flag.NewFlagSet("agent-sessions-hub", flag.ContinueOnError)
	listen := set.String("listen", configuredListen, "TCP address to listen on")
	showVersion := set.Bool("version", false, "print the build version")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	federation.RuntimeVersion = version
	return federation.RunHub(ctx, federation.HubOptions{Listen: *listen})
}

func hubListenAddress(getenv func(string) string, home string) (string, error) {
	if value := strings.TrimSpace(getenv("AGENT_SESSIONS_HUB_LISTEN")); value != "" {
		return value, nil
	}
	path := filepath.Join(home, ".config", "agent-sessions", "hub.env")
	file, err := os.Open(path) //nolint:gosec // Fixed per-user configuration path.
	if err != nil {
		if os.IsNotExist(err) {
			return ":7419", nil
		}
		return "", fmt.Errorf("open hub environment: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxHubConfigBytes+1))
	if err != nil {
		return "", fmt.Errorf("read hub environment: %w", err)
	}
	if len(body) > maxHubConfigBytes {
		return "", fmt.Errorf("hub environment exceeds bounded size %d", maxHubConfigBytes)
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "AGENT_SESSIONS_HUB_LISTEN" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value != "" {
			return value, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan hub environment: %w", err)
	}
	return ":7419", nil
}
