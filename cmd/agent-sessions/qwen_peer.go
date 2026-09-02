package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/launcher"
)

const qwenPeerReadyTimeout = 45 * time.Second

var qwenNativeSessionIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

func runQwenNativePeer(ctx context.Context, launch launcher.QwenNativeLaunch) error {
	command := exec.Command(launch.Executable, launch.Arguments...) //nolint:gosec // product executable and argv were resolved by the launcher.
	command.Dir = launch.Cwd
	command.Env = append([]string(nil), launch.Environment...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return runLauncherHeldPeer(ctx, []launcherHeldChild{{
		role: "Qwen TUI", command: command, primary: true,
	}}, func(confirmCtx context.Context) (launcherHeldIdentity, error) {
		sessionID, err := awaitQwenNativeIdentity(confirmCtx, launch.EventsPath)
		if err != nil {
			return launcherHeldIdentity{}, err
		}
		name := sessionID
		if nativeName, _, ok := qwenNativeSessionInfo(launch.QwenHome, sessionID); ok {
			name = nativeName
		}
		report := liveSessionReport{
			UUID: sessionID, Name: name, Product: connectorProductQwen,
			Groups: append([]string(nil), launch.Groups...),
		}
		return launcherHeldIdentity{
			report: report,
			call: func(callCtx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
				return qwenLauncherLiveCall(callCtx, launch.InputPath, method, params)
			},
			watch: func(watchCtx context.Context, live *liveSessionClient, report liveSessionReport) {
				startQwenNativeNameProjection(watchCtx, live, launch.QwenHome, report)
			},
		}, nil
	})
}

func awaitQwenNativeIdentity(ctx context.Context, eventsPath string) (string, error) {
	deadline := time.NewTimer(qwenPeerReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if sessionID, ok := qwenLaunchSessionID(eventsPath); ok {
			return sessionID, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", errors.New("Qwen did not emit its native session identity")
		case <-ticker.C:
		}
	}
}

func qwenLaunchSessionID(eventsPath string) (string, bool) {
	file, err := os.Open(eventsPath) //nolint:gosec // launcher-owned Qwen event stream.
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(io.LimitReader(file, 64*1024))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return "", false
	}
	var event struct {
		SessionID string `json:"sessionId"`
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type != "system" || event.Subtype != "session_start" {
		return "", false
	}
	event.SessionID = strings.TrimSpace(event.SessionID)
	return event.SessionID, qwenNativeSessionIDPattern.MatchString(event.SessionID)
}

func startQwenNativeNameProjection(
	ctx context.Context,
	live *liveSessionClient,
	home string,
	report liveSessionReport,
) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				name, _, ok := qwenNativeSessionInfo(home, report.UUID)
				if !ok || name == report.Name {
					continue
				}
				report.Name = name
				updateCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				_ = live.UpdateReport(updateCtx, report)
				cancel()
			}
		}
	}()
}

func qwenLauncherLiveCall(
	_ context.Context,
	inputPath, method string,
	params json.RawMessage,
) (json.RawMessage, error) {
	if method != "message.deliver" {
		return nil, fmt.Errorf("live session method %s is unsupported", method)
	}
	_, body, err := liveMessageRequest(params)
	if err != nil {
		return nil, err
	}
	if err := submitQwenNativeInput(inputPath, body); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}
