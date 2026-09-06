package qwen

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

const InputFileEnv = "AGENTBUS_QWEN_INPUT_FILE"

type PeerBackend struct {
	mu       sync.Mutex
	prepare  sync.Mutex
	peer     *sessionkit.Peer
	caller   *sessionkit.Caller
	identity sessionkit.PeerIdentity
	input    string
	failed   error
}

func NewPeerBackend() *PeerBackend {
	groups := []string{}
	b := &PeerBackend{input: os.Getenv(InputFileEnv)}
	b.caller = sessionkit.NewCaller(b.Call)
	if raw := os.Getenv(host.GroupsEnv); raw != "" && json.Unmarshal([]byte(raw), &groups) != nil {
		b.failed = errors.New("AGENTBUS_GROUPS must be a JSON array")
		return b
	}
	if groups == nil {
		groups = []string{}
	}
	id := strings.TrimSpace(os.Getenv(host.SessionIDEnv))
	if id == "" || b.input == "" {
		b.failed = errors.New("Qwen peer identity is unavailable; start Qwen with qwen-peer")
		return b
	}
	cwd, _ := os.Getwd()
	b.identity = sessionkit.PeerIdentity{Product: "qwen", SessionID: id, Name: first(strings.TrimSpace(os.Getenv(host.NameEnv)), id), Groups: groups, Info: map[string]any{"cwd": cwd}}
	return b
}

func (b *PeerBackend) Start() error {
	if b.failed != nil || b.peer != nil {
		return nil
	}
	identity := b.identity
	if title := nativeTitle(identity.SessionID); title != "" {
		identity.Name = title
	}
	peer, err := sessionkit.ConnectPeer(identity, b.deliver)
	if err != nil {
		return err
	}
	b.identity, b.peer = identity, peer
	return nil
}

func (b *PeerBackend) Prepare(ctx context.Context, _ json.RawMessage) error {
	b.prepare.Lock()
	defer b.prepare.Unlock()
	b.mu.Lock()
	if b.failed != nil {
		err := b.failed
		b.mu.Unlock()
		return err
	}
	peer, identity := b.peer, b.identity
	b.mu.Unlock()
	if peer == nil {
		return errors.New("Qwen peer is not connected")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-peer.Ready():
	case <-peer.Closed():
		if err := peer.Err(); err != nil {
			return err
		}
		return errors.New("Qwen peer closed before ready")
	}
	observed := identity
	if title := nativeTitle(identity.SessionID); title != "" {
		observed.Name = title
	}
	if observed.Name != identity.Name {
		if err := peer.Rehello(observed.Name, observed.Info); err != nil {
			return err
		}
		b.mu.Lock()
		b.identity = observed
		b.mu.Unlock()
	}
	return nil
}

func (b *PeerBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	peer, failed := b.peer, b.failed
	b.mu.Unlock()
	if peer == nil {
		if failed != nil {
			return nil, failed
		}
		return nil, errors.New("Qwen peer is not connected")
	}
	return peer.Call(ctx, method, params)
}

func (b *PeerBackend) Caller() *sessionkit.Caller { return b.caller }
func (b *PeerBackend) Shutdown() {
	b.mu.Lock()
	peer := b.peer
	b.peer = nil
	if b.failed == nil {
		b.failed = errors.New("Qwen peer is closed")
	}
	b.mu.Unlock()
	if peer != nil {
		peer.Shutdown()
	}
	_ = os.Remove(b.input)
}

func (b *PeerBackend) deliver(ctx context.Context, _ sessionkit.PeerIdentity, request sessionkit.DeliveryRequest) (receipt sessionkit.DeliveryReceipt, err error) {
	if err = ctx.Err(); err != nil {
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "closing"}, nil
	}
	body, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	file, err := os.OpenFile(b.input, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	err = errors.Join(json.NewEncoder(file).Encode(map[string]string{"type": "submit", "text": body}), file.Close())
	return sessionkit.DeliveryReceipt{Disposition: "injected"}, err
}

func nativeTitle(id string) string {
	home := os.Getenv("QWEN_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(user, ".qwen")
	}
	matches, _ := filepath.Glob(filepath.Join(home, "projects", "*", "chats", id+".jsonl"))
	if len(matches) != 1 {
		return ""
	}
	info, err := os.Lstat(matches[0])
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	file, err := os.Open(matches[0])
	if err != nil {
		return ""
	}
	defer file.Close()
	title := ""
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var event struct {
			SessionID     string `json:"sessionId"`
			Type          string
			Subtype       string
			SystemPayload struct {
				Title  string `json:"customTitle"`
				Source string `json:"titleSource"`
			} `json:"systemPayload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.SessionID != id {
			return ""
		}
		if event.Type == "system" && event.Subtype == "custom_title" && strings.TrimSpace(event.SystemPayload.Source) != "auto" {
			title = strings.TrimSpace(event.SystemPayload.Title)
		}
	}
	if scanner.Err() != nil {
		return ""
	}
	return title
}

func InteractivePlan(arguments, environment []string) (host.ExecPlan, error) {
	passthroughPlan, passthrough, err := host.ClassifiedInteractivePlan("qwen", arguments, environment, host.PeerIdentity{}, qwenOptionTakesValue, qwenPassthrough)
	if err != nil || passthrough {
		return passthroughPlan, err
	}
	resumeID, resumed, err := qwenResume(arguments)
	if err != nil {
		return host.ExecPlan{}, err
	}
	id := resumeID
	if !resumed {
		id, err = sessionID("")
		if err != nil {
			return host.ExecPlan{}, err
		}
	}
	environment = slices.DeleteFunc(environment, func(value string) bool {
		key, _, _ := strings.Cut(value, "=")
		return key == mcp.LaneSocketEnv || key == InputFileEnv
	})
	plan, err := host.InteractivePlan("qwen", arguments, environment, host.PeerIdentity{SessionID: id, Name: id}, qwenOptionTakesValue)
	if err != nil {
		return host.ExecPlan{}, err
	}
	if slices.ContainsFunc(plan.Args, func(argument string) bool {
		key, _, _ := strings.Cut(argument, "=")
		return key == "--session-id" || key == "--continue" || key == "-c" || key == "--fork-session"
	}) {
		return host.ExecPlan{}, errors.New("Qwen session selectors are owned by qwen-peer")
	}
	file, err := os.CreateTemp("", "agentbus-qwen-*.jsonl")
	if err != nil {
		return host.ExecPlan{}, err
	}
	path := file.Name()
	if !resumed {
		name := environmentValue(plan.Env, host.NameEnv)
		if name != "" && name != id {
			err = json.NewEncoder(file).Encode(map[string]string{"type": "submit", "text": "/rename " + name})
		}
	}
	if err = errors.Join(err, file.Close()); err != nil {
		_ = os.Remove(path)
		return host.ExecPlan{}, err
	}
	index := slices.Index(plan.Args, "--")
	if index < 0 {
		index = len(plan.Args)
	}
	managed := []string{"--chat-recording=true", "--input-file", path}
	if !resumed {
		managed = append(managed, "--session-id", id)
	}
	plan.Args = slices.Concat(plan.Args[:index], managed, plan.Args[index:])
	plan.Env = append(plan.Env, InputFileEnv+"="+path)
	if !slices.ContainsFunc(plan.Env, func(value string) bool { return strings.HasPrefix(value, host.SocketEnv+"=") }) {
		plan.Env = append(plan.Env, host.SocketEnv+"="+sessionkit.Socket())
	}
	return plan, nil
}

func qwenResume(arguments []string) (string, bool, error) {
	for index := 0; index < len(arguments); index++ {
		name, value, attached := strings.Cut(arguments[index], "=")
		if name == "--" {
			break
		}
		if name == "--resume" || name == "-r" {
			if !attached && index+1 < len(arguments) && !strings.HasPrefix(arguments[index+1], "-") {
				value = arguments[index+1]
			}
			if !qwenSessionID.MatchString(value) {
				return "", true, errors.New("qwen-peer --resume requires an exact Qwen session UUID; native titles and the picker are unavailable in managed peer mode")
			}
			return value, true, nil
		}
		if !attached && qwenOptionTakesValue(name) {
			if index+1 == len(arguments) {
				return "", false, errors.New(name + " requires a value")
			}
			index++
		}
	}
	return "", false, nil
}

var qwenSessionID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func environmentValue(environment []string, name string) string {
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && key == name {
			return value
		}
	}
	return ""
}

func qwenPassthrough(argument string) bool {
	return slices.Contains([]string{"-h", "--help", "-v", "--version", "auth", "channel", "extensions", "hooks", "mcp", "review", "serve", "sessions", "update"}, argument)
}

func qwenOptionTakesValue(argument string) bool {
	name, _, attached := strings.Cut(argument, "=")
	return !attached && slices.Contains(qwenRequiredValueOptions, name)
}

var qwenRequiredValueOptions = []string{
	"--telemetry-target", "--telemetry-otlp-endpoint", "--telemetry-otlp-protocol", "--telemetry-outfile", "--proxy",
	"-m", "--model", "--fallback-model", "-p", "--prompt", "-i", "--prompt-interactive", "--system-prompt", "--append-system-prompt", "--output-style", "--sandbox-image", "--approval-mode", "--channel", "--allowed-mcp-server-names", "--mcp-config", "--allowed-tools", "-e", "--extensions", "--include-directories", "--add-dir", "--openai-logging-dir", "--openai-api-key", "--openai-base-url", "--input-format", "-o", "--output-format", "--json-fd", "--json-file", "--json-schema", "--input-file", "--session-id", "--max-session-turns", "--max-wall-time", "--max-tool-calls", "--max-subagent-depth", "--core-tools", "--exclude-tools", "--disabled-slash-commands", "--auth-type",
}

var qwenValueOptions = append(slices.Clone(qwenRequiredValueOptions), "--worktree")

var _ mcp.Backend = (*PeerBackend)(nil)
