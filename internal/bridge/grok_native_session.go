package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const grokNativeLeaderReadyTimeout = 15 * time.Second

// GrokNativeLeaderBootstrap is the one private-leader startup used by both
// interactive peers and daemon-owned lanes. Callers choose who supervises the
// returned command; Ready owns the single authenticated startup hold.
type GrokNativeLeaderBootstrap struct {
	command      *exec.Cmd
	leaderSocket string
	bin          string
	cwd          string
	environment  []string
	diagnostics  io.Writer
	mu           sync.Mutex
	hold         *GrokNativeStartupHold
}

// NewGrokNativeLeaderBootstrap constructs Grok's exact private leader argv.
func NewGrokNativeLeaderBootstrap(
	bin, cwd, leaderSocket string,
	environment []string,
	diagnostics io.Writer,
) (*GrokNativeLeaderBootstrap, error) {
	if strings.TrimSpace(bin) == "" || strings.TrimSpace(cwd) == "" || !strings.HasPrefix(leaderSocket, "/") {
		return nil, errors.New("invalid Grok private leader configuration")
	}
	if diagnostics == nil {
		diagnostics = os.Stderr
	}
	command := exec.Command(bin, //nolint:gosec // Exact installed product binary and closed native argv.
		"--permission-mode", "default", "agent", "leader", "--leader-socket", leaderSocket,
		"--relay-on-demand", "--no-auto-update",
	)
	command.Dir = cwd
	command.Env = append([]string(nil), environment...)
	command.Stdout, command.Stderr = diagnostics, diagnostics
	configureGrokChildProcess(command)
	return &GrokNativeLeaderBootstrap{
		command: command, leaderSocket: leaderSocket, bin: bin, cwd: cwd,
		environment: append([]string(nil), environment...), diagnostics: diagnostics,
	}, nil
}

func (bootstrap *GrokNativeLeaderBootstrap) Command() *exec.Cmd {
	if bootstrap == nil {
		return nil
	}
	return bootstrap.command
}

// Start gives a daemon-owned lane the same leader command used by the peer
// runner while retaining an exact process handle for archive cleanup.
func (bootstrap *GrokNativeLeaderBootstrap) Start() (*GrokNativeLeader, error) {
	if bootstrap == nil || bootstrap.command == nil {
		return nil, errors.New("Grok private leader bootstrap is unavailable")
	}
	process, err := startGrokManagedProcess(bootstrap.command, nil)
	if err != nil {
		return nil, err
	}
	return &GrokNativeLeader{process: process}, nil
}

// Ready waits for the product socket and opens the legitimate authenticated
// client that spans the leader's startup disconnect window.
func (bootstrap *GrokNativeLeaderBootstrap) Ready(ctx context.Context) error {
	if bootstrap == nil {
		return errors.New("Grok private leader bootstrap is unavailable")
	}
	readyCtx, cancel := context.WithTimeout(ctx, grokNativeLeaderReadyTimeout)
	if err := waitForGrokNativeSocket(readyCtx, bootstrap.leaderSocket); err != nil {
		cancel()
		return err
	}
	cancel()
	hold, err := OpenGrokNativeStartupHold(
		ctx, bootstrap.bin, bootstrap.cwd, bootstrap.leaderSocket,
		bootstrap.environment, bootstrap.diagnostics,
	)
	if err != nil {
		return err
	}
	bootstrap.mu.Lock()
	if bootstrap.hold != nil {
		bootstrap.mu.Unlock()
		hold.Close()
		return errors.New("Grok private leader is already ready")
	}
	bootstrap.hold = hold
	bootstrap.mu.Unlock()
	return nil
}

func (bootstrap *GrokNativeLeaderBootstrap) Release() {
	if bootstrap == nil {
		return
	}
	bootstrap.mu.Lock()
	hold := bootstrap.hold
	bootstrap.hold = nil
	bootstrap.mu.Unlock()
	hold.Close()
}

func waitForGrokNativeSocket(ctx context.Context, path string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Grok private leader socket readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// GrokNativeLeader is the exact daemon-owned private leader process.
type GrokNativeLeader struct{ process *grokManagedProcess }

func (leader *GrokNativeLeader) Close() {
	if leader == nil || leader.process == nil {
		return
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-leader.process.done:
		return
	case <-timer.C:
	}
	stopGrokManagedProcess(leader.process, 2*time.Second)
}

// GrokNativePrimary is the one leader-connected ACP client that owns a lane's
// product session and prompt stream.
type GrokNativePrimary struct {
	client    *grokACPClient
	mu        sync.Mutex
	sessionID string
	active    *GrokNativePrompt
}

type GrokNativePromptResult struct {
	Output     string
	StopReason string
}

type GrokNativePrompt struct {
	done   chan struct{}
	mu     sync.Mutex
	output strings.Builder
	result GrokNativePromptResult
	err    error
}

func OpenGrokNativePrimary(
	ctx context.Context,
	bin, cwd, leaderSocket string,
	environment []string,
	diagnostics io.Writer,
	arguments []string,
) (*GrokNativePrimary, error) {
	clientArguments := []string{"--yolo"}
	clientArguments = append(clientArguments, arguments...)
	client, err := openGrokAuthenticatedClientWithArguments(
		ctx, bin, cwd, leaderSocket, "", environment, diagnostics, clientArguments,
	)
	if err != nil {
		return nil, err
	}
	primary := &GrokNativePrimary{client: client}
	client.setNotificationHandler(primary.handleNotification)
	return primary, nil
}

func (primary *GrokNativePrimary) OpenSession(
	ctx context.Context,
	cwd string,
	mcpServer map[string]any,
	resumeNativeID string,
) (string, error) {
	if primary == nil || primary.client == nil || strings.TrimSpace(cwd) == "" {
		return "", errors.New("Grok native primary is unavailable")
	}
	params := map[string]any{
		"cwd": cwd, "mcpServers": []any{mcpServer}, "_meta": map[string]any{"yoloMode": true},
	}
	method := "session/new"
	if resumeNativeID != "" {
		method, params["sessionId"] = "session/load", resumeNativeID
	}
	result, err := primary.client.request(ctx, method, params)
	if err != nil {
		return "", err
	}
	nativeID := defaultString(stringValue(result["sessionId"]), resumeNativeID)
	if !validSessionID(nativeID) {
		return "", errors.New("Grok ACP returned no native session identity")
	}
	if resumeNativeID != "" && nativeID != resumeNativeID {
		return "", errors.New("Grok ACP loaded a different native session identity")
	}
	primary.mu.Lock()
	if primary.sessionID != "" {
		primary.mu.Unlock()
		return "", errors.New("Grok native primary already owns a session")
	}
	primary.sessionID = nativeID
	primary.mu.Unlock()
	return nativeID, nil
}

func (primary *GrokNativePrimary) SetModel(ctx context.Context, model string) error {
	return primary.configure(ctx, "session/set_model", "modelId", model)
}

func (primary *GrokNativePrimary) SetMode(ctx context.Context, mode string) error {
	return primary.configure(ctx, "session/set_mode", "modeId", mode)
}

func (primary *GrokNativePrimary) configure(ctx context.Context, method, key, value string) error {
	if primary == nil || primary.client == nil || strings.TrimSpace(value) == "" {
		return errors.New("Grok native session configuration is unavailable")
	}
	primary.mu.Lock()
	sessionID, active := primary.sessionID, primary.active
	primary.mu.Unlock()
	if !validSessionID(sessionID) || active != nil {
		return errors.New("Grok native session is not idle")
	}
	_, err := primary.client.request(ctx, method, map[string]any{"sessionId": sessionID, key: value})
	return err
}

func (primary *GrokNativePrimary) StartPrompt(ctx context.Context, prompt string) (*GrokNativePrompt, error) {
	if primary == nil || primary.client == nil || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("Grok native prompt is unavailable")
	}
	primary.mu.Lock()
	if !validSessionID(primary.sessionID) || primary.active != nil {
		primary.mu.Unlock()
		return nil, errors.New("Grok native session is not idle")
	}
	nativePrompt := &GrokNativePrompt{done: make(chan struct{})}
	primary.active = nativePrompt
	sessionID := primary.sessionID
	primary.mu.Unlock()
	future, err := primary.client.requestAsync(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
	})
	if err != nil {
		primary.mu.Lock()
		if primary.active == nativePrompt {
			primary.active = nil
		}
		primary.mu.Unlock()
		return nil, err
	}
	go func() {
		<-future.done
		nativePrompt.mu.Lock()
		nativePrompt.result.Output = nativePrompt.output.String()
		nativePrompt.result.StopReason = stringValue(future.result["stopReason"])
		nativePrompt.err = future.err
		nativePrompt.mu.Unlock()
		primary.mu.Lock()
		if primary.active == nativePrompt {
			primary.active = nil
		}
		primary.mu.Unlock()
		close(nativePrompt.done)
	}()
	return nativePrompt, nil
}

func (primary *GrokNativePrimary) Cancel() error {
	if primary == nil || primary.client == nil {
		return errors.New("Grok native primary is unavailable")
	}
	primary.mu.Lock()
	sessionID, active := primary.sessionID, primary.active
	primary.mu.Unlock()
	if !validSessionID(sessionID) || active == nil {
		return errors.New("Grok native session has no active prompt")
	}
	return primary.client.notifyCancel(map[string]any{"sessionId": sessionID})
}

func (primary *GrokNativePrimary) Close() {
	if primary != nil && primary.client != nil {
		primary.client.close()
		primary.client = nil
	}
}

func (primary *GrokNativePrimary) handleNotification(message map[string]any) {
	method := stringValue(message["method"])
	if method != "session/update" && method != "x.ai/session/update" && method != "_x.ai/session/update" {
		return
	}
	params := mapValue(message["params"])
	primary.mu.Lock()
	if sessionID := stringValue(params["sessionId"]); sessionID != "" && sessionID != primary.sessionID {
		primary.mu.Unlock()
		return
	}
	active := primary.active
	primary.mu.Unlock()
	if active == nil {
		return
	}
	update := mapValue(params["update"])
	if len(update) == 0 {
		update = mapValue(params["sessionUpdate"])
	}
	if stringValue(update["sessionUpdate"]) != "agent_message_chunk" {
		return
	}
	text := defaultString(stringValue(mapValue(update["content"])["text"]), stringValue(update["text"]))
	if text != "" {
		active.mu.Lock()
		active.output.WriteString(text)
		active.mu.Unlock()
	}
}

func (prompt *GrokNativePrompt) Wait(ctx context.Context) (GrokNativePromptResult, error) {
	if prompt == nil {
		return GrokNativePromptResult{}, errors.New("Grok native prompt is unavailable")
	}
	select {
	case <-ctx.Done():
		return GrokNativePromptResult{}, ctx.Err()
	case <-prompt.done:
		prompt.mu.Lock()
		defer prompt.mu.Unlock()
		return prompt.result, prompt.err
	}
}
