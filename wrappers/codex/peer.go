package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
	"github.com/antst/agent-sessions/wrappers/mcp"
)

type PeerBackend struct {
	mu            sync.Mutex
	prepareMu     sync.Mutex
	app           *appClient
	process       *exec.Cmd
	processDone   chan error
	peer          *sessionkit.Peer
	caller        *sessionkit.Caller
	identity      sessionkit.PeerIdentity
	groups        []string
	fixedID       string
	requestedName string
	named         string
}

var _ mcp.Backend = (*PeerBackend)(nil)

func NewPeerBackend(ctx context.Context) (*PeerBackend, error) {
	groups := []string{}
	if raw := os.Getenv(host.GroupsEnv); raw != "" && json.Unmarshal([]byte(raw), &groups) != nil {
		return nil, errors.New("AGENTBUS_GROUPS must be a JSON array")
	}
	b := &PeerBackend{groups: groups, fixedID: strings.TrimSpace(os.Getenv(host.SessionIDEnv)), requestedName: strings.TrimSpace(os.Getenv(host.NameEnv)), processDone: make(chan error, 1)}
	command, input, output, err := startPeerApp()
	if err != nil {
		return nil, err
	}
	b.process = command
	b.app = newAppClient(input, output, nil, b.nativeFailure)
	b.caller = sessionkit.NewCaller(b.Call)
	go func() { b.processDone <- command.Wait() }()
	if err = b.app.initialize(ctx, "Agentbus Codex Peer"); err != nil {
		b.Shutdown()
		return nil, err
	}
	if b.fixedID != "" {
		if err = b.applyName(ctx, b.fixedID); err != nil {
			b.Shutdown()
			return nil, err
		}
		identity, observeErr := b.observe(ctx, b.fixedID)
		if observeErr != nil {
			b.Shutdown()
			return nil, observeErr
		}
		if err = b.connect(ctx, identity); err != nil {
			b.Shutdown()
			return nil, err
		}
	}
	return b, nil
}

func startPeerApp() (*exec.Cmd, io.WriteCloser, io.Reader, error) {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, nil, err
		}
		home = filepath.Join(user, ".codex")
	}
	command := exec.Command("codex", "app-server", "proxy", "--sock", filepath.Join(home, "app-server-control", "app-server-control.sock"))
	command.Stderr = os.Stderr
	command.Env = slices.DeleteFunc(os.Environ(), func(value string) bool {
		key, _, _ := strings.Cut(value, "=")
		return key == mcp.LaneSocketEnv || strings.HasPrefix(key, "AGENTBUS_")
	})
	input, err := command.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err = command.Start(); err != nil {
		_ = input.Close()
		return nil, nil, nil, err
	}
	return command, input, output, nil
}

func (b *PeerBackend) Caller() *sessionkit.Caller { return b.caller }

func (b *PeerBackend) Prepare(ctx context.Context, meta json.RawMessage) error {
	b.prepareMu.Lock()
	defer b.prepareMu.Unlock()
	var observed struct {
		ThreadID string `json:"threadId"`
	}
	if len(meta) != 0 && string(meta) != "null" && json.Unmarshal(meta, &observed) != nil {
		return errors.New("Codex peer metadata is invalid")
	}
	id := strings.TrimSpace(observed.ThreadID)
	b.mu.Lock()
	if b.fixedID != "" {
		if id != "" && id != b.fixedID {
			b.mu.Unlock()
			return errors.New("Codex peer thread identity changed")
		}
		id = b.fixedID
	}
	current := b.identity.SessionID
	b.mu.Unlock()
	if id == "" {
		return errors.New("Codex peer identity is unavailable; start Codex with codex-peer")
	}
	if err := b.applyName(ctx, id); err != nil {
		return err
	}
	identity, err := b.observe(ctx, id)
	if err != nil {
		return err
	}
	b.mu.Lock()
	peer, old := b.peer, b.identity
	b.mu.Unlock()
	if peer == nil {
		return b.connect(ctx, identity)
	}
	if current != identity.SessionID {
		if err = peer.Replace(ctx, identity); err != nil {
			return err
		}
		b.mu.Lock()
		b.identity = identity
		b.mu.Unlock()
		return nil
	}
	if old.Name == identity.Name && old.Info["cwd"] == identity.Info["cwd"] {
		return nil
	}
	if err = peer.Rehello(identity.Name, identity.Info); err != nil {
		return err
	}
	b.mu.Lock()
	b.identity = identity
	b.mu.Unlock()
	return nil
}

func (b *PeerBackend) applyName(ctx context.Context, id string) error {
	if b.named == id || b.requestedName == "" {
		return nil
	}
	if err := b.app.call(ctx, "thread/name/set", map[string]string{"threadId": id, "name": b.requestedName}, &struct{}{}); err != nil {
		return err
	}
	b.named = id
	return nil
}

func (b *PeerBackend) connect(ctx context.Context, identity sessionkit.PeerIdentity) error {
	peer, err := sessionkit.ConnectPeer(identity, b.deliver)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		peer.Shutdown()
		return ctx.Err()
	case <-peer.Ready():
	}
	b.mu.Lock()
	b.peer, b.identity = peer, identity
	b.mu.Unlock()
	return nil
}

func (b *PeerBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	peer := b.peer
	b.mu.Unlock()
	if peer == nil {
		return nil, errors.New("Codex peer identity is unavailable; start Codex with codex-peer")
	}
	return peer.Call(ctx, method, params)
}

func (b *PeerBackend) nativeFailure(error) {
	b.mu.Lock()
	peer := b.peer
	b.mu.Unlock()
	if peer != nil {
		peer.Shutdown()
	}
}

func (b *PeerBackend) observe(ctx context.Context, id string) (sessionkit.PeerIdentity, error) {
	var result threadReply
	if err := b.app.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &result); err != nil {
		return sessionkit.PeerIdentity{}, err
	}
	if result.Thread.ID != id {
		return sessionkit.PeerIdentity{}, fmt.Errorf("Codex App Server returned thread %q, expected %q", result.Thread.ID, id)
	}
	return sessionkit.PeerIdentity{Product: "codex", SessionID: id, Name: first(strings.TrimSpace(result.Thread.Name), id), Groups: append([]string{}, b.groups...), Info: map[string]any{"cwd": strings.TrimSpace(result.Thread.Cwd)}}, nil
}

func (b *PeerBackend) deliver(ctx context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	message, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	b.mu.Lock()
	id := b.identity.SessionID
	b.mu.Unlock()
	var read threadReply
	if err = b.app.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &read); err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	status := statusType(read.Thread.Status)
	if status == "notLoaded" {
		var resumed threadReply
		if err = b.app.call(ctx, "thread/resume", map[string]any{"threadId": id, "excludeTurns": true}, &resumed); err != nil {
			return sessionkit.DeliveryReceipt{}, err
		}
		status = statusType(resumed.Thread.Status)
	}
	if status == "active" {
		var page struct {
			Data []nativeTurn `json:"data"`
		}
		if err = b.app.call(ctx, "thread/turns/list", map[string]any{"threadId": id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &page); err != nil {
			return sessionkit.DeliveryReceipt{}, err
		}
		if len(page.Data) != 1 || page.Data[0].ID == "" || page.Data[0].Status != "inProgress" {
			return sessionkit.DeliveryReceipt{}, errors.New("Codex active turn is unavailable")
		}
		var steered struct {
			TurnID string `json:"turnId"`
		}
		err = b.app.call(ctx, "turn/steer", map[string]any{"threadId": id, "expectedTurnId": page.Data[0].ID, "input": textInput(message)}, &steered)
		if err == nil && steered.TurnID != page.Data[0].ID {
			err = errors.New("Codex steered a different turn")
		}
	} else {
		var started turnReply
		err = b.app.call(ctx, "turn/start", map[string]any{"threadId": id, "input": textInput(message)}, &started)
		if err == nil && started.Turn.ID == "" {
			err = errors.New("Codex did not start a delivery turn")
		}
	}
	return sessionkit.DeliveryReceipt{Disposition: "injected"}, err
}

func statusType(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "inProgress" {
			return "active"
		}
		return text
	}
	var object struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &object)
	return object.Type
}

func (b *PeerBackend) Shutdown() {
	b.mu.Lock()
	peer, app, done := b.peer, b.app, b.processDone
	b.peer = nil
	b.mu.Unlock()
	if peer != nil {
		peer.Shutdown()
		<-peer.Closed()
	}
	if app != nil {
		_ = app.close()
	}
	if done != nil {
		<-done
	}
}
