// grouped_peer_fixture is a test-only product adapter used by the federation
// integration suite. It exercises the same registration and AgentFrame APIs as
// Codex, Claude, and Grok adapters without launching a model client.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
)

type fixtureOutput struct{ mu sync.Mutex }

func (o *fixtureOutput) write(value any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func main() {
	runtimeDir := flag.String("runtime-dir", "", "host agent runtime directory")
	sessionID := flag.String("session", "", "stable session id")
	product := flag.String("product", "codex", "peer product")
	name := flag.String("name", "fixture", "peer name")
	groupsCSV := flag.String("groups", "", "comma-separated explicit groups")
	flag.Parse()
	if *runtimeDir == "" || *sessionID == "" {
		fmt.Fprintln(os.Stderr, "runtime-dir and session are required")
		os.Exit(2)
	}
	groups := []string{}
	for _, group := range strings.Split(*groupsCSV, ",") {
		if group = strings.TrimSpace(group); group != "" {
			groups = append(groups, group)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	socket := filepath.Join(*runtimeDir, "fixture-"+*sessionID+".sock")
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		panic(err)
	}
	defer func() { _ = listener.Close(); _ = os.Remove(socket) }()
	output := &fixtureOutput{}
	go acceptFrames(ctx, listener, output)
	preference := federator.ResolvePreferencesRequest{
		SessionID: *sessionID, Product: *product, Groups: groups, GroupsSpecified: true,
		AlwaysApprove: false, AlwaysApproveSpecified: true,
	}
	if _, err := federator.ResolveSessionPreferences(*runtimeDir, preference); err != nil {
		panic(err)
	}
	registration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: *sessionID, Product: *product,
		Name: *name, Status: "idle", PermissionMode: "default", Cwd: mustGetwd(),
		PID: os.Getpid(), ProcStart: federator.ProcessStart(os.Getpid()), Socket: socket,
		LifecyclePID: os.Getpid(), LifecycleProcStart: federator.ProcessStart(os.Getpid()),
		StartedAt: time.Now().UnixMilli(),
	}
	if err := registerUntil(ctx, *runtimeDir, registration); err != nil {
		panic(err)
	}
	defer func() { _ = federator.UnregisterPeer(*runtimeDir, registration) }()
	output.write(map[string]any{"event": "ready", "session_id": *sessionID})
	go refreshRegistration(ctx, *runtimeDir, registration)
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	readCommands(ctx, *runtimeDir, *sessionID, output)
}

func acceptFrames(ctx context.Context, listener net.Listener, output *fixtureOutput) {
	go func() { <-ctx.Done(); _ = listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				var outer map[string]any
				if json.Unmarshal(scanner.Bytes(), &outer) != nil {
					continue
				}
				message, _ := outer["message"].(map[string]any)
				content, _ := message["content"].(string)
				frame, err := federator.DecodeAgentFrameBody(content)
				if err == nil {
					output.write(map[string]any{"event": "delivery", "frame": frame})
				}
			}
		}()
	}
}

func registerUntil(ctx context.Context, runtimeDir string, registration federator.PeerRegistration) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := federator.RegisterPeer(runtimeDir, registration); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func refreshRegistration(ctx context.Context, runtimeDir string, registration federator.PeerRegistration) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = federator.RegisterPeer(runtimeDir, registration)
		}
	}
}

func readCommands(ctx context.Context, runtimeDir, sessionID string, output *fixtureOutput) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command struct {
			ID    string               `json:"id"`
			Frame federator.AgentFrame `json:"frame"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
			output.write(map[string]any{"event": "result", "id": command.ID, "error": err.Error()})
			continue
		}
		result, err := federator.RouteAgentFrame(runtimeDir, sessionID, command.Frame)
		if err != nil {
			output.write(map[string]any{"event": "result", "id": command.ID, "error": err.Error()})
		} else {
			output.write(map[string]any{"event": "result", "id": command.ID, "result": result})
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func mustGetwd() string {
	cwd, _ := os.Getwd()
	return cwd
}
