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
	Resident  bool   `json:"resident"`
}

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
	row, err := b.roster(ctx, id)
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
		row.Title = wanted
	}
	b.identity = sessionkit.PeerIdentity{Product: "grok", SessionID: id, Name: first(row.Title, wanted, id), Groups: groups, Info: map[string]any{"cwd": row.Cwd}}
	peer, err := sessionkit.ConnectPeer(b.identity, b.deliver)
	if err != nil {
		return stop(err)
	}
	b.peer = peer
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

func (b *PeerBackend) Prepare(ctx context.Context) error {
	b.mu.Lock()
	id, old, observer, peer := b.identity.SessionID, b.identity, b.observer, b.peer
	b.mu.Unlock()
	row, err := roster(ctx, observer, id)
	if err != nil {
		return err
	}
	name := first(row.Title, id)
	if name == old.Name && row.Cwd == old.Info["cwd"] {
		return nil
	}
	b.mu.Lock()
	b.identity.Name, b.identity.Info = name, map[string]any{"cwd": row.Cwd}
	b.mu.Unlock()
	return peer.Rehello(name, map[string]any{"cwd": row.Cwd})
}

func (b *PeerBackend) deliver(ctx context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	b.mu.Lock()
	id, observer := b.identity.SessionID, b.observer
	b.mu.Unlock()
	row, err := roster(ctx, observer, id)
	if err != nil {
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "no_leader"}, nil
	}
	message, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	if err = observer.interject(ctx, id, request.MessageID, message); err != nil {
		return sessionkit.DeliveryReceipt{Disposition: "rejected", Reason: "no_leader"}, nil
	}
	if row.Activity == "working" {
		return sessionkit.DeliveryReceipt{Disposition: "injected"}, nil
	}
	return sessionkit.DeliveryReceipt{Disposition: "queued_for_next_turn"}, nil
}

func (b *PeerBackend) roster(ctx context.Context, id string) (peerSession, error) {
	return roster(ctx, b.observer, id)
}

func roster(ctx context.Context, observer *acpClient, id string) (peerSession, error) {
	if observer == nil {
		return peerSession{}, errors.New("no_leader")
	}
	var reply struct {
		Result struct {
			Sessions []peerSession `json:"sessions"`
		} `json:"result"`
	}
	if err := observer.request(ctx, "_x.ai/sessions/list", map[string]any{}, &reply); err != nil {
		return peerSession{}, err
	}
	for _, session := range reply.Result.Sessions {
		if session.SessionID == id && session.Resident {
			return session, nil
		}
	}
	return peerSession{}, errors.New("no_leader")
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
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		if argument == "--no-leader" || strings.HasPrefix(argument, "--leader-socket") {
			return "", errors.New("grok-peer requires the default leader")
		}
		name, value, attached := strings.Cut(argument, "=")
		if name == "--session-id" || name == "-s" || name == "--resume" || name == "-r" {
			if !attached {
				if index+1 == len(arguments) || strings.HasPrefix(arguments[index+1], "-") {
					return "", errors.New(argument + " requires a value")
				}
				value = arguments[index+1]
			}
			return value, nil
		}
		if grokOptionTakesValue(argument) {
			index++
		}
	}
	return "", nil
}

func grokOptionTakesValue(argument string) bool {
	name, _, attached := strings.Cut(argument, "=")
	if attached {
		return false
	}
	return slices.Contains([]string{"-g", "--group", "--agent", "--agents", "--allow", "--cwd", "--debug-file", "--deny", "--disallowed-tools", "--json-schema", "--leader-socket", "-m", "--model", "--max-turns", "--output-format", "--permission-mode", "--prompt-file", "--prompt-json", "-r", "--resume", "--reasoning-effort", "--effort", "--rules", "-s", "--session-id", "--sandbox", "--system-prompt-override", "--tools", "--worktree-ref", "--ref"}, name)
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6], value[8] = value[6]&0x0f|0x40, value[8]&0x3f|0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
