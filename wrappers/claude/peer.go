package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/wrappers/host"
)

const messagingSocketEnv = "CLAUDE_CODE_MESSAGING_SOCKET"

var readAgents = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "claude", "agents", "--json").Output()
}

type PeerBackend struct {
	mu       sync.Mutex
	peer     *sessionkit.Peer
	caller   *sessionkit.Caller
	identity sessionkit.PeerIdentity
	groups   []string
	parent   int
	failed   error
}

func NewPeerBackend(ctx context.Context) (*PeerBackend, error) {
	var groups []string
	if raw := os.Getenv(host.GroupsEnv); raw != "" && json.Unmarshal([]byte(raw), &groups) != nil {
		return nil, errors.New("AGENTBUS_GROUPS must be a JSON array")
	}
	b := &PeerBackend{groups: groups, parent: os.Getppid()}
	id := strings.TrimSpace(os.Getenv(host.SessionIDEnv))
	if id != "" {
		name := first(strings.TrimSpace(os.Getenv(host.NameEnv)), id)
		b.identity = sessionkit.PeerIdentity{Product: "claude", SessionID: id, Name: name, Groups: groups, Info: map[string]any{}}
	}
	if id == "" {
		observed, err := b.observe(ctx)
		if err != nil {
			b.failed = err
			b.caller = sessionkit.NewCaller(b.Call)
			return b, nil
		}
		b.identity = observed
	}
	peer, err := sessionkit.ConnectPeer(b.identity, b.deliver)
	if err != nil {
		return nil, err
	}
	b.peer = peer
	b.caller = peer.Caller
	return b, nil
}

func (b *PeerBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	peer, failed := b.peer, b.failed
	b.mu.Unlock()
	if peer == nil {
		return nil, failed
	}
	return peer.Call(ctx, method, params)
}

func (b *PeerBackend) Prepare(ctx context.Context) error {
	b.mu.Lock()
	if b.peer == nil {
		err := b.failed
		b.mu.Unlock()
		return err
	}
	if identity, err := b.observe(ctx); err == nil && (identity.SessionID != b.identity.SessionID || identity.Name != b.identity.Name || identity.Info["cwd"] != b.identity.Info["cwd"]) {
		old := b.identity
		b.identity = identity
		if identity.SessionID != old.SessionID {
			err = b.peer.Replace(ctx, identity)
		} else {
			err = b.peer.Rehello(identity.Name, identity.Info)
		}
		if err != nil {
			b.mu.Unlock()
			return err
		}
	}
	b.mu.Unlock()
	return nil
}

func (b *PeerBackend) Caller() *sessionkit.Caller { return b.caller }

func (b *PeerBackend) Shutdown() {
	if b.peer != nil {
		b.peer.Shutdown()
	}
}

func (b *PeerBackend) observe(ctx context.Context) (sessionkit.PeerIdentity, error) {
	payload, err := readAgents(ctx)
	if err != nil {
		return sessionkit.PeerIdentity{}, errors.New("Claude peer identity is unavailable; start Claude with claude-peer")
	}
	return activeSession(payload, b.parent, b.groups)
}

func activeSession(payload []byte, parent int, groups []string) (sessionkit.PeerIdentity, error) {
	var sessions []struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Cwd       string `json:"cwd"`
		PID       int    `json:"pid"`
	}
	if json.Unmarshal(payload, &sessions) != nil {
		return sessionkit.PeerIdentity{}, errors.New("Claude peer identity is unavailable; start Claude with claude-peer")
	}
	var found sessionkit.PeerIdentity
	for _, session := range sessions {
		id := strings.TrimSpace(session.SessionID)
		if session.PID != parent || session.Kind != "interactive" || id == "" {
			continue
		}
		candidate := sessionkit.PeerIdentity{Product: "claude", SessionID: id, Name: first(strings.TrimSpace(session.Name), id), Groups: append([]string(nil), groups...), Info: map[string]any{"cwd": strings.TrimSpace(session.Cwd)}}
		if found.SessionID != "" && (found.SessionID != candidate.SessionID || found.Name != candidate.Name || found.Info["cwd"] != candidate.Info["cwd"]) {
			return sessionkit.PeerIdentity{}, errors.New("Claude peer identity is ambiguous")
		}
		found = candidate
	}
	if found.SessionID == "" {
		return found, errors.New("Claude peer identity is unavailable; start Claude with claude-peer")
	}
	return found, nil
}

func (b *PeerBackend) deliver(_ context.Context, request sessionkit.DeliveryRequest) (sessionkit.DeliveryReceipt, error) {
	message, err := host.RenderNativeMessage(request)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	path := strings.TrimSpace(os.Getenv(messagingSocketEnv))
	if path == "" {
		return sessionkit.DeliveryReceipt{}, errors.New("Claude messaging socket is unavailable")
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		return sessionkit.DeliveryReceipt{}, err
	}
	defer connection.Close()
	body := map[string]any{"msgV": 1, "msg_id": request.MessageID, "type": "user", "priority": "next", "from": "agentbus", "message": map[string]any{"role": "user", "content": message}}
	err = json.NewEncoder(connection).Encode(body)
	return sessionkit.DeliveryReceipt{Disposition: "injected"}, err
}
