package sessionkit

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

const defaultPeerReconnectInterval = 2 * time.Second

var peerReconnectInterval = defaultPeerReconnectInterval
var dialPeer = net.Dial

type DeliverFunc func(context.Context, DeliveryRequest) (DeliveryReceipt, error)

type Peer struct {
	Caller        *Caller
	mu            sync.Mutex
	identity      PeerIdentity
	generation    uint64
	deliver       DeliverFunc
	socket        string
	wire          *rpc.Conn
	ctx           context.Context
	cancel        context.CancelFunc
	ready, closed chan struct{}
}

func ConnectPeer(identity PeerIdentity, deliver DeliverFunc) (*Peer, error) {
	socket, _, err := sessionEnvironment(false)
	if err != nil {
		return nil, err
	}
	identity, err = snapshotIdentity(identity)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Peer{identity: identity, deliver: deliver, socket: socket, ctx: ctx, cancel: cancel, ready: make(chan struct{}), closed: make(chan struct{})}
	p.Caller = newCaller(p.call)
	go p.connect()
	return p, nil
}

func (p *Peer) Ready() <-chan struct{}  { return p.ready }
func (p *Peer) Closed() <-chan struct{} { return p.closed }

func (p *Peer) Rehello(name string, info map[string]any) error {
	p.mu.Lock()
	next := p.identity
	p.mu.Unlock()
	next.Name, next.Info = name, info
	next, err := snapshotIdentity(next)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.mu.Unlock()
		return &ProtocolError{Code: protocol.Superseded, Message: "superseded"}
	}
	p.identity = next
	p.generation++
	wire, generation := p.wire, p.generation
	p.mu.Unlock()
	if wire == nil {
		return errNotConnected
	}
	err = p.hello(wire, next, generation)
	if err != nil && wire.Context().Err() != nil {
		return errNotConnected
	}
	return err
}

func (p *Peer) Shutdown() {
	p.cancel()
	if wire := p.connection(); wire != nil {
		_ = wire.Close()
	}
}

func (p *Peer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var result json.RawMessage
	err := p.call(ctx, method, params, &result)
	return result, err
}

func (p *Peer) call(ctx context.Context, method string, params, result any) error {
	wire := p.connection()
	if wire == nil {
		return errNotConnected
	}
	return wire.Call(ctx, method, params, result)
}

func (p *Peer) connection() *rpc.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wire
}

func (p *Peer) connect() {
	defer close(p.closed)
	ready := p.ready
	for {
		fd, err := dialPeer("unix", p.socket)
		if err == nil {
			var wire *rpc.Conn
			wire = rpc.New(fd, true, func(_ context.Context, request *rpc.Request) { p.handle(wire, request) })
			p.mu.Lock()
			identity, generation := p.identity, p.generation
			p.mu.Unlock()
			if p.hello(wire, identity, generation) == nil {
				if ready != nil {
					close(ready)
					ready = nil
				}
				<-wire.Done()
				p.mu.Lock()
				if p.wire == wire {
					p.wire = nil
				}
				p.mu.Unlock()
			}
			_ = wire.Close()
		}
		if p.ctx.Err() != nil {
			return
		}
		select {
		case <-time.After(peerReconnectInterval):
		case <-p.ctx.Done():
			return
		}
	}
}

func (p *Peer) hello(wire *rpc.Conn, identity PeerIdentity, generation uint64) error {
	for {
		if err := wire.Call(p.ctx, "session.hello", identity, &struct{}{}); err != nil {
			return err
		}
		p.mu.Lock()
		if p.ctx.Err() != nil {
			p.mu.Unlock()
			return rpc.ErrClosed
		}
		if generation == p.generation {
			p.wire = wire
			p.mu.Unlock()
			return nil
		}
		identity, generation = p.identity, p.generation
		p.mu.Unlock()
	}
}

func (p *Peer) handle(wire *rpc.Conn, request *rpc.Request) {
	switch request.Method {
	case "session.superseded":
		p.cancel()
		go func() { _ = wire.Result(request, struct{}{}); _ = wire.Close() }()
	case "message.deliver":
		go func() {
			receipt, err := p.deliver(wire.Context(), *request.Params.(*DeliveryRequest))
			if err != nil {
				receipt = DeliveryReceipt{Disposition: "rejected", Reason: err.Error()}
			}
			if wire.Result(request, receipt) != nil {
				_ = wire.Close()
			}
		}()
	}
}

var errNotConnected = &ProtocolError{Code: protocol.NotConnected, Message: "not_connected"}

func snapshotIdentity(identity PeerIdentity) (PeerIdentity, error) {
	identity.Protocol = 1
	raw, err := protocol.EncodeParams("session.hello", identity)
	var snapshot PeerIdentity
	if err == nil {
		err = json.Unmarshal(raw, &snapshot)
	}
	return snapshot, err
}
