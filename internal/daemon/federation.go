package daemon

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/antst/sessionbus/internal/federation"
)

// Federation is the one outbound network component owned by a daemon
// generation. It prevents a second in-process host authority while exposing
// only the roster, delivery, and remote-lane operations needed by daemon
// workflows.
type Federation struct {
	host    *federation.EmbeddedHost
	running atomic.Bool
}

// NewFederation constructs the generation-scoped outbound component.
func NewFederation(options federation.EmbeddedHostOptions) (*Federation, error) {
	host, err := federation.NewEmbeddedHost(options)
	if err != nil {
		return nil, err
	}
	return &Federation{host: host}, nil
}

// Run owns the sole host loop until daemon cancellation.
func (f *Federation) Run(ctx context.Context) error {
	if f == nil || f.host == nil {
		return errors.New("daemon federation component is unavailable")
	}
	if !f.running.CompareAndSwap(false, true) {
		return errors.New("daemon federation component is already running")
	}
	defer f.running.Store(false)
	return f.host.Run(ctx)
}

// RemotePeers returns the current remote session roster.
func (f *Federation) RemotePeers() []federation.Peer {
	if f == nil || f.host == nil {
		return nil
	}
	return f.host.RemotePeers()
}

// RemoteHosts returns the current connected host roster.
func (f *Federation) RemoteHosts() []federation.Host {
	if f == nil || f.host == nil {
		return nil
	}
	return f.host.RemoteHosts()
}

// Connected reports whether the embedded host currently has a live hub stream.
func (f *Federation) Connected() bool {
	if f == nil || f.host == nil {
		return false
	}
	return f.host.Connected()
}

// Send performs one acknowledged cross-host delivery.
func (f *Federation) Send(ctx context.Context, source, target federation.Peer, messageID, content, group string) error {
	if f == nil || f.host == nil {
		return errors.New("daemon federation component is unavailable")
	}
	return f.host.Send(ctx, source, target, messageID, content, group)
}

// RunRemoteLane performs one bounded cross-host lane operation.
func (f *Federation) RunRemoteLane(ctx context.Context, request federation.RemoteLaneRequest) (federation.RemoteLaneResult, error) {
	if f == nil || f.host == nil {
		return federation.RemoteLaneResult{}, errors.New("daemon federation component is unavailable")
	}
	return f.host.RunRemoteLane(ctx, request)
}
