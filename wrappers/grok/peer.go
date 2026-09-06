package grok

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

type peerSession struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Cwd       string `json:"cwd"`
	Activity  string `json:"activity"`
	Resident  *bool  `json:"resident"`
	Yolo      *bool  `json:"yolo"`
}

var errNoLeader = errors.New("no_leader")

type PeerBackend struct {
	mu       sync.Mutex
	peer     *sessionkit.Peer
	observer *acpClient
	process  *nativeProcess
	identity sessionkit.PeerIdentity
}

func NewPeerBackend(ctx context.Context) (*PeerBackend, error) {
	id := strings.TrimSpace(os.Getenv(host.SessionIDEnv))
	if id == "" {
		return nil, errors.New("Grok peer identity is unavailable; start Grok with grok-peer")
	}
	groups := []string{}
	if raw := os.Getenv(host.GroupsEnv); raw != "" && json.Unmarshal([]byte(raw), &groups) != nil {
		return nil, errors.New("AGENTBUS_GROUPS must be a JSON array")
	}
	cmd := command("grok", "agent", "--leader", "stdio")
	cmd.Env, cmd.Stderr, cmd.SysProcAttr = nativeEnvironment(), os.Stderr, &syscall.SysProcAttr{Setpgid: true}
	input, _ := cmd.StdinPipe()
	output, _ := cmd.StdoutPipe()
	process, err := startNative(cmd)
	if err != nil {
		return nil, err
	}
	observer := newACPClient(input, output, nil)
	stop := func(err error) (*PeerBackend, error) {
		observer.close()
		_ = process.cmd.Process.Signal(syscall.SIGKILL)
		<-process.done
		return nil, err
	}
	if err = initializeACP(ctx, observer); err != nil {
		return stop(err)
	}
	b := &PeerBackend{observer: observer, process: process}
	wanted := strings.TrimSpace(os.Getenv(host.NameEnv))
	row, err := roster(ctx, observer, id)
	if err != nil {
		return stop(err)
	}
	if wanted != "" && wanted != id && wanted != row.Title {
		var renamed struct {
			Success bool `json:"success"`
		}
		if err = observer.request(ctx, "_x.ai/session/rename", map[string]string{"sessionId": id, "title": wanted}, &renamed); err != nil || !renamed.Success {
			return stop(errors.New("Grok peer title is unavailable"))
		}
		if row, err = roster(ctx, observer, id); err != nil || row.Title != wanted {
			return stop(errors.New("Grok peer title was not confirmed"))
		}
	}
	b.identity = sessionkit.PeerIdentity{Product: "grok", SessionID: id, Name: first(row.Title, wanted, id), Groups: groups, Info: map[string]any{"cwd": row.Cwd}}
	peer, err := sessionkit.ConnectPeer(b.identity, b.deliver)
	if err != nil {
		return stop(err)
	}
	b.peer = peer
	select {
	case <-peer.Ready():
	case <-ctx.Done():
		peer.Shutdown()
		return stop(ctx.Err())
	}
	return b, nil
}

func (b *PeerBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	peer := b.peer
	b.mu.Unlock()
	if peer == nil {
		return nil, errors.New("not connected")
	}
	return peer.Call(ctx, method, params)
}

func (b *PeerBackend) Caller() *sessionkit.Caller { return b.peer.Caller }

func (b *PeerBackend) Prepare(ctx context.Context, _ json.RawMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, old, observer, peer := b.identity.SessionID, b.identity, b.observer, b.peer
	row, err := roster(ctx, observer, id)
	if err != nil {
		return err
	}
	name := first(row.Title, id)
	if name == old.Name && row.Cwd == old.Info["cwd"] {
		return nil
	}
	info := map[string]any{"cwd": row.Cwd}
	if err := peer.Rehello(name, info); err != nil {
		return err
	}
	b.identity.Name, b.identity.Info = name, info
	return nil
}

func (b *PeerBackend) deliver(ctx context.Context, identity sessionkit.PeerIdentity, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	b.mu.Lock()
	observer := b.observer
	b.mu.Unlock()
	row, err := roster(ctx, observer, identity.SessionID)
	if err != nil {
		if errors.Is(err, errNoLeader) {
			return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "no_leader"}, nil
		}
		return sessionkit.DeliveryReceipt{}, err
	}
	message, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	if err = observer.interject(ctx, identity.SessionID, request.MessageID, message); err != nil {
		return sessionkit.DeliveryReceipt{}, fmt.Errorf("Grok interject: %w", err)
	}
	if row.Activity == "working" {
		return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
	}
	return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func roster(ctx context.Context, observer *acpClient, id string) (peerSession, error) {
	if observer == nil {
		return peerSession{}, errors.New("Grok observer is unavailable")
	}
	var reply struct {
		Result struct {
			Sessions []peerSession `json:"sessions"`
		} `json:"result"`
	}
	if err := observer.request(ctx, "_x.ai/sessions/list", map[string]any{}, &reply); err != nil {
		return peerSession{}, err
	}
	return exactRoster(reply.Result.Sessions, id)
}

func exactRoster(sessions []peerSession, id string) (peerSession, error) {
	var found peerSession
	matches := 0
	for _, session := range sessions {
		if session.SessionID == id {
			matches++
			found = session
		}
	}
	if matches == 0 {
		return peerSession{}, errNoLeader
	}
	if matches != 1 {
		return peerSession{}, fmt.Errorf("Grok roster returned %d exact rows for %s", matches, id)
	}
	if found.Resident == nil {
		return peerSession{}, errors.New("exact Grok session roster row has no resident state")
	}
	if !*found.Resident || slices.Contains([]string{"completed", "dormant", "dead"}, found.Activity) {
		return peerSession{}, fmt.Errorf("Grok session %s is not live", id)
	}
	if found.Yolo == nil {
		return peerSession{}, errors.New("live Grok roster row has no yolo state")
	}
	if !slices.Contains([]string{"working", "needs_input", "idle"}, found.Activity) {
		return peerSession{}, fmt.Errorf("live Grok roster row has unsupported activity %q", found.Activity)
	}
	return found, nil
}

func (b *PeerBackend) Shutdown() {
	b.mu.Lock()
	peer, observer, process := b.peer, b.observer, b.process
	b.peer, b.observer, b.process = nil, nil, nil
	b.mu.Unlock()
	if peer != nil {
		peer.Shutdown()
	}
	if observer != nil {
		observer.close()
	}
	if process != nil {
		_ = process.cmd.Process.Signal(syscall.SIGKILL)
		<-process.done
	}
}

func InteractivePlan(arguments, environment []string) (host.ExecPlan, error) {
	if grokPassthrough(arguments) {
		return host.ExecPlan{Path: "grok", Args: slices.Clone(arguments), Env: slices.Clone(environment)}, nil
	}
	id, err := interactiveIdentity(arguments)
	if err != nil {
		return host.ExecPlan{}, err
	}
	if id == "" {
		id, err = newSessionID()
		if err != nil {
			return host.ExecPlan{}, err
		}
		arguments = append([]string{"--session-id", id}, arguments...)
	}
	if !slices.Contains(arguments, "--leader") {
		index := slices.Index(arguments, "--")
		if index < 0 {
			index = len(arguments)
		}
		arguments = slices.Insert(arguments, index, "--leader")
	}
	if !slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, host.SocketEnv+"=") }) {
		environment = append(environment, host.SocketEnv+"="+sessionkit.Socket())
	}
	return host.InteractivePlan("grok", arguments, environment, host.PeerIdentity{SessionID: id, Name: id}, grokOptionTakesValue)
}

func interactiveIdentity(arguments []string) (string, error) {
	identity := ""
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		if argument == "--leader" || strings.HasPrefix(argument, "--leader=") || argument == "--no-leader" || strings.HasPrefix(argument, "--no-leader=") || strings.HasPrefix(argument, "--leader-socket") {
			return "", errors.New("grok-peer requires the default leader")
		}
		if argument == "--continue" || strings.HasPrefix(argument, "--continue=") || argument == "-c" || argument == "--fork-session" || strings.HasPrefix(argument, "--fork-session=") {
			return "", fmt.Errorf("%s cannot identify an exact managed Grok session", argument)
		}
		name, value, attached := strings.Cut(argument, "=")
		resume := name == "--resume" || name == "--load" || name == "-r"
		fresh := name == "--session-id" || name == "-s"
		if !attached && strings.HasPrefix(argument, "-r") && argument != "-r" {
			resume, value, attached = true, strings.TrimPrefix(argument, "-r"), true
		}
		if !attached && strings.HasPrefix(argument, "-s") && argument != "-s" {
			fresh, value, attached = true, strings.TrimPrefix(argument, "-s"), true
		}
		if resume || fresh {
			if !attached {
				if index+1 == len(arguments) || strings.HasPrefix(arguments[index+1], "-") {
					return "", errors.New(argument + " requires a value")
				}
				index++
				value = arguments[index]
			}
			if strings.TrimSpace(value) == "" {
				return "", errors.New(argument + " requires a value")
			}
			if identity != "" {
				return "", errors.New("Grok session identity was specified more than once")
			}
			identity = value
			continue
		}
		if grokOptionTakesValue(argument) {
			index++
		}
	}
	return identity, nil
}

var grokCommands = map[string]bool{
	"agent": true, "clone": true, "completions": true, "dashboard": true,
	"doctor": true, "du": true, "disk-usage": true, "export": true,
	"help": true, "inspect": true, "leader": true, "login": true,
	"logout": true, "mcp": true, "memory": true, "models": true,
	"plugin": true, "sessions": true, "setup": true, "trace": true,
	"update": true, "version": true, "v": true, "worktree": true,
	"wrap": true,
}

func grokPassthrough(arguments []string) bool {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			return false
		}
		if argument == "-h" || argument == "--help" || argument == "-v" || argument == "--version" {
			return true
		}
		if !strings.HasPrefix(argument, "-") {
			return grokCommands[argument]
		}
		if grokOptionTakesValue(argument) {
			index++
		}
	}
	return false
}

func grokOptionTakesValue(argument string) bool {
	name, _, attached := strings.Cut(argument, "=")
	if attached {
		return false
	}
	return slices.Contains([]string{"-g", "--group", "--agent", "--agents", "--allow", "--cwd", "--debug-file", "--deny", "--disallowed-tools", "--json-schema", "--leader-socket", "-m", "--model", "--max-turns", "--output-format", "--permission-mode", "--prompt-file", "--prompt-json", "-r", "--resume", "--load", "--reasoning-effort", "--effort", "--rules", "-s", "--session-id", "--sandbox", "--system-prompt-override", "--tools", "--worktree-ref", "--ref"}, name)
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6], value[8] = value[6]&0x0f|0x40, value[8]&0x3f|0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
