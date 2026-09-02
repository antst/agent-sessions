package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	"github.com/antst/agent-sessions/internal/launcher"
)

const grokPeerReadyTimeout = 15 * time.Second

func runGrokNativePeer(ctx context.Context, launch launcher.GrokNativeLaunch) error {
	var startupHold *bridge.GrokNativeStartupHold
	defer func() { startupHold.Close() }()

	leader := exec.Command(launch.Executable, launch.LeaderArguments...) //nolint:gosec // resolved product executable and structured argv.
	leader.Dir = launch.Cwd
	leader.Env = append([]string(nil), launch.LeaderEnvironment...)
	leader.Stdout, leader.Stderr = os.Stderr, os.Stderr
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	tui := exec.Command(launch.Executable, launch.TUIArguments...) //nolint:gosec // resolved product executable and structured argv.
	tui.Dir = launch.Cwd
	tui.Env = append([]string(nil), launch.TUIEnvironment...)
	tui.Stdin, tui.Stdout, tui.Stderr = os.Stdin, os.Stdout, os.Stderr

	return runLauncherHeldPeer(ctx, []launcherHeldChild{
		{
			role: "Grok private leader", command: leader,
			ready: func(readyCtx context.Context) error {
				if err := waitForGrokSocket(readyCtx, launch.LeaderSocket, grokPeerReadyTimeout); err != nil {
					return err
				}
				var err error
				startupHold, err = bridge.OpenGrokNativeStartupHold(
					readyCtx, launch.Executable, launch.Cwd, launch.LeaderSocket,
					launch.LeaderEnvironment, os.Stderr,
				)
				return err
			},
		},
		{role: "Grok TUI", command: tui, primary: true},
	}, func(confirmCtx context.Context) (launcherHeldIdentity, error) {
		observer, sessionID, err := awaitGrokNativeIdentity(confirmCtx, launch)
		if err != nil {
			return launcherHeldIdentity{}, err
		}
		defer observer.Close()
		startupHold.Close()
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

func waitForGrokSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("socket readiness timed out")
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
