package sessionkit

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

const peerReconnectInterval = 2 * time.Second

type DeliverFunc func(context.Context, DeliveryRequest) (DeliveryReceipt, error)

type Peer struct {
	Caller        *Caller
	mu            sync.Mutex
	identity      PeerIdentity
	deliver       DeliverFunc
	socket        string
	wire          *rpc.Conn
	ctx           context.Context
	cancel        context.CancelFunc
	ready, closed chan struct{}
	readyOnce     sync.Once
}

func ConnectPeer(identity PeerIdentity, deliver DeliverFunc) (*Peer, error) {
	socket, _, err := sessionEnvironment(false)
	if err != nil {
		return nil, err
	}
	identity = cloneIdentity(identity)
	if _, err = protocol.EncodeParams("session.hello", identity); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Peer{identity: identity, deliver: deliver, socket: socket, ctx: ctx, cancel: cancel, ready: make(chan struct{}), closed: make(chan struct{})}
	p.Caller = newCaller(p.Call)
	go p.connect()
	return p, nil
}

func (p *Peer) Ready() <-chan struct{}  { return p.ready }
func (p *Peer) Closed() <-chan struct{} { return p.closed }

func (p *Peer) Rehello(identity PeerIdentity) error {
	next := cloneIdentity(identity)
	if _, err := protocol.EncodeParams("session.hello", next); err != nil {
		return err
	}
	if p.ctx.Err() != nil {
		return &ProtocolError{Code: protocol.Superseded, Message: "superseded"}
	}
	p.mu.Lock()
	p.identity = next
	p.mu.Unlock()
	wire := p.connection()
	if wire == nil {
		return errNotConnected
	}
	err := wire.Call(context.Background(), "session.hello", next, &struct{}{})
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
	wire := p.connection()
	if wire == nil {
		return nil, errNotConnected
	}
	var result json.RawMessage
	err := wire.Call(ctx, method, params, &result)
	if err != nil && wire.Context().Err() != nil {
		err = errNotConnected
	}
	return result, err
}

func (p *Peer) connection() *rpc.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wire
}

func (p *Peer) connect() {
	defer close(p.closed)
	for {
		fd, err := net.Dial("unix", p.socket)
		if err == nil {
			var wire *rpc.Conn
			wire = rpc.New(fd, true, func(_ context.Context, request *rpc.Request) { p.handle(wire, request) })
			p.mu.Lock()
			identity := p.identity
			p.mu.Unlock()
			if wire.Call(p.ctx, "session.hello", identity, &struct{}{}) == nil {
				p.mu.Lock()
				if p.ctx.Err() != nil {
					p.mu.Unlock()
					_ = wire.Close()
					return
				}
				p.wire = wire
				p.mu.Unlock()
				p.readyOnce.Do(func() { close(p.ready) })
				<-wire.Done()
				p.mu.Lock()
				if p.wire == wire {
					p.wire = nil
				}
				p.mu.Unlock()
			}
			_ = wire.Close()
		}
		select {
		case <-time.After(peerReconnectInterval):
		case <-p.ctx.Done():
			return
		}
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

func cloneIdentity(identity PeerIdentity) PeerIdentity {
	identity.Protocol = 1
	identity.Groups = append([]string{}, identity.Groups...)
	identity.Info = maps.Clone(identity.Info)
	return identity
}
