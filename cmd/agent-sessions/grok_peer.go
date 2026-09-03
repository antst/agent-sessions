package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	"github.com/antst/agent-sessions/internal/launcher"
	grokproduct "github.com/antst/agent-sessions/internal/products/grok"
)

const grokPeerReadyTimeout = 15 * time.Second

func runGrokNativePeer(ctx context.Context, launch launcher.GrokNativeLaunch) error {
	startup, err := bridge.NewGrokNativeLeaderBootstrap(
		launch.Executable, launch.Cwd, launch.LeaderSocket, launch.LeaderEnvironment, os.Stderr,
	)
	if err != nil {
		return err
	}
	defer startup.Release()
	leader := startup.Command()

	tui := exec.Command(launch.Executable, launch.TUIArguments...) //nolint:gosec // resolved product executable and structured argv.
	tui.Dir = launch.Cwd
	tui.Env = append([]string(nil), launch.TUIEnvironment...)
	tui.Stdin, tui.Stdout, tui.Stderr = os.Stdin, os.Stdout, os.Stderr

	return runLauncherHeldPeer(ctx, []launcherHeldChild{
		{
			role: "Grok private leader", command: leader,
			ready: func(readyCtx context.Context) error {
				return startup.Ready(readyCtx)
			},
		},
		{role: "Grok TUI", command: tui, primary: true},
	}, func(confirmCtx context.Context) (launcherHeldIdentity, error) {
		observer, sessionID, err := awaitGrokNativeIdentity(confirmCtx, launch)
		if err != nil {
			return launcherHeldIdentity{}, err
		}
		defer observer.Close()
		startup.Release()
		name := ""
		if launch.RequestedName != "" {
			if err := observer.Rename(confirmCtx, launch.RequestedName); err != nil {
				return launcherHeldIdentity{}, fmt.Errorf("rename Grok session: %w", err)
			}
		}
		name, err = observer.SessionName(confirmCtx)
		if err != nil {
			return launcherHeldIdentity{}, fmt.Errorf("read Grok session name: %w", err)
		}
		if launch.RequestedName != "" && name != launch.RequestedName {
			return launcherHeldIdentity{}, fmt.Errorf("Grok accepted session name %q instead of %q", name, launch.RequestedName)
		}
		if strings.TrimSpace(name) == "" {
			name = sessionID
		}
		report := liveSessionReport{
			UUID: sessionID, Name: name, Product: connectorProductGrok,
			Groups: append([]string(nil), launch.Groups...),
		}
		return launcherHeldIdentity{report: report, call: func(
			callCtx context.Context, method string, params json.RawMessage,
		) (json.RawMessage, error) {
			return grokLauncherLiveCall(callCtx, launch, report, method, params)
		}}, nil
	})
}

func awaitGrokNativeIdentity(
	ctx context.Context,
	launch launcher.GrokNativeLaunch,
) (*bridge.GrokNativeObserver, string, error) {
	deadline := time.NewTimer(grokPeerReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		var observer *bridge.GrokNativeObserver
		var sessionID string
		if launch.LateBoundResume {
			observer, sessionID, lastErr = bridge.OpenGrokNativeSelectionObserver(
				ctx, launch.Executable, launch.Cwd, launch.LeaderSocket,
				launch.SessionID, launch.LeaderEnvironment, os.Stderr,
			)
		} else {
			observer, lastErr = bridge.OpenGrokNativeObserver(
				ctx, launch.Executable, launch.Cwd, launch.LeaderSocket,
				launch.SessionID, launch.LeaderEnvironment, os.Stderr,
			)
			sessionID = launch.SessionID
		}
		if lastErr == nil {
			return observer, sessionID, nil
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-deadline.C:
			return nil, "", fmt.Errorf("Grok did not confirm its native session: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func grokLauncherLiveCall(
	ctx context.Context,
	launch launcher.GrokNativeLaunch,
	report liveSessionReport,
	method string,
	params json.RawMessage,
) (json.RawMessage, error) {
	if method != "message.deliver" {
		return nil, fmt.Errorf("live session method %s is unsupported", method)
	}
	messageID, body, err := liveMessageRequest(params)
	if err != nil {
		return nil, err
	}
	observer, err := bridge.OpenGrokNativeObserver(
		ctx, launch.Executable, launch.Cwd, launch.LeaderSocket,
		report.UUID, launch.LeaderEnvironment, os.Stderr,
	)
	if err != nil {
		return nil, err
	}
	defer observer.Close()
	if err := observer.Interject(ctx, messageID, body); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

type BridgeFactoryConfig struct {
	Executable     string
	HostExecutable string
	Diagnostics    io.Writer
}

type grokBridgeFactory struct{ config BridgeFactoryConfig }

func newGrokBridgeFactory(config BridgeFactoryConfig) (grokproduct.NativeFactory, error) {
	if strings.TrimSpace(config.Executable) == "" || strings.TrimSpace(config.HostExecutable) == "" {
		return nil, errors.New("Grok native factory requires product and host executables")
	}
	if config.Diagnostics == nil {
		config.Diagnostics = os.Stderr
	}
	return &grokBridgeFactory{config: config}, nil
}

func (factory *grokBridgeFactory) Open(ctx context.Context, request grokproduct.NativeOpenRequest) (grokproduct.NativeSession, error) {
	lifetime, cancel := context.WithCancel(context.Background())
	root, err := os.MkdirTemp("", "agent-sessions-grok-lane-")
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create Grok lane launch directory: %w", err)
	}
	fail := func(primary *bridge.GrokNativePrimary, observer *bridge.GrokNativeObserver, leader *bridge.GrokNativeLeader, cause error) (grokproduct.NativeSession, error) {
		cancel()
		if observer != nil {
			observer.Close()
		}
		if primary != nil {
			primary.Close()
		}
		if leader != nil {
			leader.Close()
		}
		_ = os.RemoveAll(root)
		return nil, cause
	}
	socket := filepath.Join(root, "leader.sock")
	environment := append([]string(nil), request.Environment...)
	startup, err := bridge.NewGrokNativeLeaderBootstrap(
		factory.config.Executable, request.Cwd, socket, environment, factory.config.Diagnostics,
	)
	if err != nil {
		return fail(nil, nil, nil, err)
	}
	leader, err := startup.Start()
	if err != nil {
		return fail(nil, nil, nil, err)
	}
	if err := startup.Ready(lifetime); err != nil {
		return fail(nil, nil, leader, err)
	}
	defer startup.Release()
	primary, err := bridge.OpenGrokNativePrimary(
		lifetime, factory.config.Executable, request.Cwd, socket, environment, factory.config.Diagnostics,
		request.Arguments,
	)
	if err != nil {
		return fail(nil, nil, leader, err)
	}
	nativeID, err := primary.OpenSession(ctx, request.Cwd, grokLaneMCPServer(factory.config.HostExecutable, request), request.ResumeNativeID)
	if err != nil {
		return fail(primary, nil, leader, err)
	}
	observer, err := bridge.OpenGrokNativeObserver(
		lifetime, factory.config.Executable, request.Cwd, socket, nativeID, environment, factory.config.Diagnostics,
	)
	if err != nil {
		return fail(primary, nil, leader, err)
	}
	if request.ResumeNativeID == "" {
		if err := observer.Rename(ctx, request.Name); err != nil {
			return fail(primary, observer, leader, fmt.Errorf("rename Grok lane session: %w", err))
		}
		name, err := observer.SessionName(ctx)
		if err != nil {
			return fail(primary, observer, leader, fmt.Errorf("confirm Grok lane session name: %w", err))
		}
		if name != request.Name {
			return fail(primary, observer, leader, fmt.Errorf("Grok accepted session name %q instead of %q", name, request.Name))
		}
	}
	startup.Release()
	return &bridgeSession{
		nativeID: nativeID, primary: primary, observer: observer, leader: leader, root: root, cancel: cancel,
	}, nil
}

type bridgeSession struct {
	nativeID string
	primary  *bridge.GrokNativePrimary
	observer *bridge.GrokNativeObserver
	leader   *bridge.GrokNativeLeader
	root     string
	cancel   context.CancelFunc
	once     sync.Once
}

func (session *bridgeSession) NativeID() string { return session.nativeID }

func (session *bridgeSession) SetModel(ctx context.Context, model string) error {
	return session.primary.SetModel(ctx, model)
}

func (session *bridgeSession) SetMode(ctx context.Context, mode string) error {
	return session.primary.SetMode(ctx, mode)
}

func (session *bridgeSession) StartPrompt(ctx context.Context, prompt string) (grokproduct.NativePrompt, error) {
	nativePrompt, err := session.primary.StartPrompt(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return bridgePrompt{prompt: nativePrompt}, nil
}

func (session *bridgeSession) Interject(ctx context.Context, messageID, message string) error {
	return session.observer.Interject(ctx, messageID, message)
}

func (session *bridgeSession) Cancel() error { return session.primary.Cancel() }

func (session *bridgeSession) Close() {
	if session == nil {
		return
	}
	session.once.Do(func() {
		session.cancel()
		session.observer.Close()
		session.primary.Close()
		session.leader.Close()
		_ = os.RemoveAll(session.root)
	})
}

type bridgePrompt struct{ prompt *bridge.GrokNativePrompt }

func (prompt bridgePrompt) Wait(ctx context.Context) (grokproduct.NativePromptResult, error) {
	result, err := prompt.prompt.Wait(ctx)
	return grokproduct.NativePromptResult{Output: result.Output, StopReason: result.StopReason}, err
}

func grokLaneMCPServer(hostExecutable string, request grokproduct.NativeOpenRequest) map[string]any {
	environment := map[string]string{
		"AGENT_SESSIONS_SESSION_ID":      request.LaneID,
		"AGENT_SESSIONS_PRODUCT":         grokproduct.ProductID,
		"AGENT_SESSIONS_LANE_CAPABILITY": request.Capability,
		"AGENT_SESSIONS_HOST_BINARY":     hostExecutable,
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]any, 0, len(names))
	for _, name := range names {
		values = append(values, map[string]any{"name": name, "value": environment[name]})
	}
	return map[string]any{
		"name": "agent_sessions", "command": hostExecutable,
		"args": []any{"connector", grokproduct.ProductID}, "env": values,
	}
}
