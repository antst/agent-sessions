package bridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/socketpath"
)

const (
	grokLaunchTokenEnv      = "AGENT_SESSIONS_GROK_LAUNCH_TOKEN"
	grokSessionIDEnv        = "AGENT_SESSIONS_GROK_SESSION_ID"
	grokACPStartupTimeout   = 15 * time.Second
	grokACPInterjectTimeout = 30 * time.Second
)

type grokHostPaths struct {
	Root          string
	LaunchDir     string
	LeaderSocket  string
	ControlSocket string
}

// GrokHostSocketPaths returns the compact, per-launch paths shared by the
// launcher and host. launchToken is never placed on disk verbatim.
func GrokHostSocketPaths(runtimeDir, launchToken string) (leader, control string, err error) {
	if !validGrokLaunchToken(launchToken) {
		return "", "", errors.New("invalid Grok launch token")
	}
	paths := grokRuntimePaths(runtimeDir, os.Getuid(), launchToken)
	return paths.LeaderSocket, paths.ControlSocket, nil
}

func grokRuntimePaths(runtimeDir string, uid int, launchToken string) grokHostPaths {
	return grokRuntimePathsForKey(runtimeDir, uid, sessionKey(launchToken))
}

func grokRuntimePathsForKey(runtimeDir string, uid int, launchKey string) grokHostPaths {
	root := filepath.Join(runtimeDir, fmt.Sprintf("agent-sessions-grok-%d", uid))
	// Prefer the caller's runtime directory, but compact before any socket is
	// created when the longest per-launch address would exceed sun_path.
	root = socketpath.PreferRoot(
		root,
		filepath.Join("/tmp", fmt.Sprintf("asg-%d", uid)),
		filepath.Join("g-"+strings.Repeat("0", 20), "control.sock"),
	)
	launchDir := filepath.Join(root, "g-"+launchKey)
	return grokHostPaths{
		Root:          root,
		LaunchDir:     launchDir,
		LeaderSocket:  filepath.Join(launchDir, "leader.sock"),
		ControlSocket: filepath.Join(launchDir, "control.sock"),
	}
}

func validGrokLaunchToken(value string) bool {
	if len(value) < 32 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func grokTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type grokManagedProcess struct {
	cmd         *exec.Cmd
	procStart   string
	done        chan struct{}
	mu          sync.Mutex
	err         error
	diagnostics *grokProcessDiagnostics
}

func startGrokManagedProcess(command *exec.Cmd, diagnostics *grokProcessDiagnostics) (*grokManagedProcess, error) {
	configureGrokChildProcess(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	procStart, err := captureProcessStart(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("capture Grok child identity: %w", err)
	}
	managed := &grokManagedProcess{cmd: command, procStart: procStart, done: make(chan struct{}), diagnostics: diagnostics}
	go func() {
		err := command.Wait()
		managed.mu.Lock()
		managed.err = err
		managed.mu.Unlock()
		close(managed.done)
	}()
	return managed, nil
}

func (p *grokManagedProcess) waitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *grokManagedProcess) attributedError(role string, cause error) error {
	// All callers with a started process stop or observe it first. Use a bounded
	// final join because stdout EOF can precede a last stderr write, while an
	// unverifiable process identity must never hang host shutdown indefinitely.
	joined := p == nil || p.done == nil
	if p != nil && p.done != nil {
		timer := time.NewTimer(grokDiagnosticJoinTimeout)
		select {
		case <-p.done:
			joined = true
			if cause == nil {
				cause = p.waitError()
			}
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	if !joined {
		if p == nil || p.diagnostics == nil {
			return fmt.Errorf("%s; managed process join incomplete; private diagnostics unavailable", role)
		}
		return p.diagnostics.safeError(role, false)
	}
	if cause == nil {
		cause = errors.New("process exited unexpectedly")
	}
	if p == nil || p.diagnostics == nil {
		return fmt.Errorf("%s; private diagnostics unavailable", role)
	}
	p.diagnostics.recordFailure(cause)
	return p.diagnostics.safeError(role, true)
}

type grokACPClient struct {
	process   *grokManagedProcess
	stdin     io.WriteCloser
	responses chan map[string]any
	interject chan map[string]any
	notifyMu  sync.RWMutex
	notify    func(map[string]any)
	readDone  chan struct{}
	readMu    sync.Mutex
	readErr   error
	requestMu sync.Mutex
	writeMu   sync.Mutex
	nextID    int64
}

func (c *grokACPClient) readError() error {
	if c == nil {
		return io.EOF
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readErr == nil {
		return io.EOF
	}
	return c.readErr
}

type grokRosterState struct {
	name           string
	permissionMode string
	status         string
	fromPush       bool
	generation     uint64
	authorityLost  bool
}

var errGrokRosterAuthorityLost = errors.New("live Grok roster authority lost")

type grokRPCError struct {
	Code    int
	Message string
	Data    any
}

func (e *grokRPCError) Error() string {
	if e.Code == 0 {
		return "Grok ACP: " + e.Message
	}
	return fmt.Sprintf("Grok ACP error %d: %s", e.Code, e.Message)
}

func (e *grokRPCError) diagnosticDetail() string {
	if e == nil {
		return ""
	}
	value, _ := e.Data.(string)
	if value == "" {
		if object, ok := e.Data.(map[string]any); ok {
			value = stringValue(object["message"])
		}
	}
	value = strings.Join(strings.Fields(strings.ToValidUTF8(value, "�")), " ")
	const maxRunes = 512
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes]) + "…"
	}
	return value
}

func newGrokACPClient(
	process *grokManagedProcess,
	stdin io.WriteCloser,
	stdout io.ReadCloser,
	sessionID string,
	generation uint64,
	rosterUpdates chan grokRosterState,
) *grokACPClient {
	client := &grokACPClient{
		process: process, stdin: stdin, responses: make(chan map[string]any, 32),
		interject: make(chan map[string]any, 32), readDone: make(chan struct{}),
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), maxFrameBytes)
		for scanner.Scan() {
			var message map[string]any
			if json.Unmarshal(scanner.Bytes(), &message) != nil {
				continue
			}
			if message["id"] == nil {
				client.notifyMu.RLock()
				notify := client.notify
				client.notifyMu.RUnlock()
				if notify != nil {
					notify(message)
				}
				switch stringValue(message["method"]) {
				case "_x.ai/session/interjection":
					select {
					case client.interject <- message:
					case <-client.readDone:
						return
					}
				case "_x.ai/sessions/changed", "x.ai/sessions/changed":
					if state, ok := grokRosterNotificationState(message, sessionID); ok {
						state.generation = generation
						publishLatestGrokRosterState(rosterUpdates, state)
					}
				}
				continue
			}
			select {
			case client.responses <- message:
			case <-client.readDone:
				return
			}
		}
		client.readMu.Lock()
		client.readErr = scanner.Err()
		client.readMu.Unlock()
		close(client.readDone)
	}()
	return client
}

func (c *grokACPClient) setNotificationHandler(handler func(map[string]any)) {
	c.notifyMu.Lock()
	c.notify = handler
	c.notifyMu.Unlock()
}

func publishLatestGrokRosterState(updates chan grokRosterState, state grokRosterState) {
	select {
	case updates <- state:
		return
	default:
	}
	select {
	case <-updates:
	default:
	}
	select {
	case updates <- state:
	default:
	}
}

func (c *grokACPClient) requestInterjection(ctx context.Context, sessionID, messageID, text string) error {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.nextID++
	id := c.nextID
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "_x.ai/interject",
		"params": map[string]any{"sessionId": sessionID, "text": text, "interjectionId": messageID},
	})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write Grok ACP _x.ai/interject: %w", err)
	}
	for {
		select {
		case notification := <-c.interject:
			params, _ := notification["params"].(map[string]any)
			if stringValue(params["sessionId"]) == sessionID && stringValue(params["interjectionId"]) == messageID {
				return nil
			}
		case response := <-c.responses:
			if int64Value(response["id"]) != id {
				continue
			}
			if raw, ok := response["error"].(map[string]any); ok {
				return &grokRPCError{
					Code: intValue(raw["code"]), Message: defaultString(stringValue(raw["message"]), "request rejected"), Data: raw["data"],
				}
			}
			result, _ := response["result"].(map[string]any)
			inner, _ := result["result"].(map[string]any)
			if stringValue(inner["status"]) != "queued" {
				return errors.New("grok ACP interjection returned no queued acknowledgement")
			}
			// Grok 1.0.4 returns queued even when the resident actor's mailbox
			// is closed. Only the actor's matching notification proves that it
			// began handling this immutable interjection id.
		case <-c.readDone:
			c.readMu.Lock()
			readErr := c.readErr
			c.readMu.Unlock()
			if readErr == nil {
				readErr = io.EOF
			}
			return fmt.Errorf("read Grok ACP interjection acknowledgement: %w", readErr)
		case <-ctx.Done():
			return fmt.Errorf("wait for Grok actor interjection acknowledgement: %w", ctx.Err())
		}
	}
}

func (c *grokACPClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.nextID++
	id := c.nextID
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("write Grok ACP %s: %w", method, err)
	}
	for {
		select {
		case response := <-c.responses:
			if int64Value(response["id"]) != id {
				// A prior timed-out request may finish later. Never confuse it with
				// the current request.
				continue
			}
			if raw, ok := response["error"].(map[string]any); ok {
				return nil, &grokRPCError{
					Code: intValue(raw["code"]), Message: defaultString(stringValue(raw["message"]), "request rejected"), Data: raw["data"],
				}
			}
			result, _ := response["result"].(map[string]any)
			return result, nil
		case <-c.readDone:
			c.readMu.Lock()
			readErr := c.readErr
			c.readMu.Unlock()
			if readErr == nil {
				readErr = io.EOF
			}
			return nil, fmt.Errorf("read Grok ACP %s: %w", method, readErr)
		case <-ctx.Done():
			return nil, fmt.Errorf("grok ACP %s: %w", method, ctx.Err())
		}
	}
}

func (c *grokACPClient) close() {
	_ = c.stdin.Close()
	stopGrokManagedProcess(c.process, 2*time.Second)
}

func (c *grokACPClient) notifyCancel(params map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "session/cancel", "params": params,
	})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write grok ACP session/cancel notification: %w", err)
	}
	return nil
}
