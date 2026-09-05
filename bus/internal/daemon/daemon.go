package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/antst/agent-sessions/bus/internal/rpc"
	"github.com/antst/agent-sessions/bus/internal/structuredprocess"
)

const maxPendingWorkerCalls = 256

type Config struct {
	SocketPath string
	TablePath  string
	Host       string
	Products   []string
}

type live struct {
	wire           *rpc.Conn
	fd             net.Conn
	worker         bool
	committed      bool
	sessionID      string
	name           string
	product        string
	groups         []string
	declaredGroups []string
	privateGroup   string
	info           map[string]any
	identityCtx    context.Context
	cancelIdentity context.CancelFunc
	child          *structuredprocess.Process
	outbound       chan *workerCall
}

type source struct {
	sessionID string
	name      string
	product   string
	groups    []string
	private   string
	ctx       context.Context
}

type pendingRun struct{}

type workerCall struct {
	method  string
	params  any
	result  any
	seen    func() error
	started chan (<-chan error)
}

type reservation struct {
	product string
	hello   chan workerStart
	process *structuredprocess.Process
}

type Daemon struct {
	config   Config
	host     string
	table    *table
	listener net.Listener

	mapsMutex     sync.Mutex
	connections   map[string]*live
	accepted      map[*live]bool
	pending       map[string]*pendingRun
	rowLocks      map[string]*sync.Mutex
	reservations  map[string]*reservation
	reservedNames map[string]bool
	closing       bool
	spawnWG       sync.WaitGroup

	closeOnce  sync.Once
	shutdown   chan struct{}
	done       chan struct{}
	acceptDone chan struct{}
}

func Start(config Config) (*Daemon, error) {
	if config.Host == "" {
		config.Host = "local"
	}
	if !validHost(config.Host) {
		return nil, errors.New("invalid agentbus host")
	}
	seenProducts := map[string]bool{}
	for _, product := range config.Products {
		if !validHost(product) {
			return nil, errors.New("invalid advertised product")
		}
		if seenProducts[product] {
			return nil, errors.New("duplicate advertised product")
		}
		seenProducts[product] = true
	}
	if len(config.Products) == 0 {
		config.Products = nil
	}
	t, err := openTable(config.TablePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(config.SocketPath), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(config.SocketPath)
	listener, err := net.Listen("unix", config.SocketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(config.SocketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	d := &Daemon{config: config, host: config.Host, table: t, listener: listener,
		connections: map[string]*live{}, accepted: map[*live]bool{}, pending: map[string]*pendingRun{}, rowLocks: map[string]*sync.Mutex{}, reservations: map[string]*reservation{}, reservedNames: map[string]bool{},
		shutdown: make(chan struct{}), done: make(chan struct{}), acceptDone: make(chan struct{})}
	go d.accept()
	return d, nil
}

func (d *Daemon) Done() <-chan struct{} { return d.done }

func (d *Daemon) accept() {
	defer close(d.acceptDone)
	for {
		fd, err := d.listener.Accept()
		if err != nil {
			return
		}
		connection := &live{fd: fd}
		ready := make(chan struct{})
		connection.wire = rpc.New(fd, false, func(ctx context.Context, request *rpc.Request) {
			<-ready
			d.handle(ctx, connection, request)
		})
		d.mapsMutex.Lock()
		closing := d.closing
		d.accepted[connection] = true
		d.mapsMutex.Unlock()
		close(ready)
		if closing {
			_ = connection.wire.Close()
			return
		}
		go func() {
			<-connection.wire.Done()
			d.disconnected(connection)
		}()
	}
}

func (d *Daemon) Close() error {
	d.closeOnce.Do(func() {
		_ = d.listener.Close()
		d.mapsMutex.Lock()
		d.closing = true
		close(d.shutdown)
		connections := make([]*live, 0, len(d.accepted))
		children := make([]*structuredprocess.Process, 0, len(d.accepted)+len(d.reservations))
		for connection := range d.accepted {
			connections = append(connections, connection)
			children = append(children, connection.child)
		}
		for _, reservation := range d.reservations {
			children = append(children, reservation.process)
		}
		d.mapsMutex.Unlock()
		for _, connection := range connections {
			_ = connection.fd.Close()
		}
		for _, child := range children {
			child.KillAndWait()
		}
		<-d.acceptDone
		d.spawnWG.Wait()
		_ = os.Remove(d.config.SocketPath)
		close(d.done)
	})
	return nil
}

func (d *Daemon) rowLock(name string) *sync.Mutex {
	d.mapsMutex.Lock()
	defer d.mapsMutex.Unlock()
	lock := d.rowLocks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		d.rowLocks[name] = lock
	}
	return lock
}

func (d *Daemon) disconnected(connection *live) {
	defer func() {
		d.mapsMutex.Lock()
		delete(d.accepted, connection)
		d.mapsMutex.Unlock()
	}()
	d.mapsMutex.Lock()
	worker, committed, id := connection.worker, connection.committed, connection.sessionID
	d.mapsMutex.Unlock()
	if !worker && id != "" {
		d.removePeer(connection, id)
		return
	}
	if !worker || !committed {
		return
	}
	value, ok := d.table.get(id)
	if !ok {
		connection.child.KillAndWait()
		return
	}
	lock := d.rowLock(value.SessionID)
	lock.Lock()
	d.mapsMutex.Lock()
	if d.connections[id] == connection {
		delete(d.connections, id)
		delete(d.pending, id)
	}
	d.mapsMutex.Unlock()
	connection.child.KillAndWait()
	lock.Unlock()
}

func (d *Daemon) removePeer(connection *live, id string) {
	d.mapsMutex.Lock()
	cancel := connection.cancelIdentity
	if d.connections[id] == connection {
		delete(d.connections, id)
	}
	d.mapsMutex.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (connection *live) startSender() {
	connection.outbound = make(chan *workerCall, maxPendingWorkerCalls)
	go func() {
		for {
			select {
			case call := <-connection.outbound:
				call.started <- connection.wire.Begin(call.method, call.params, call.result, call.seen)
			case <-connection.wire.Done():
				return
			}
		}
	}()
}

func (connection *live) enqueue(method string, params, result any, seen func() error) (*workerCall, bool) {
	call := &workerCall{method: method, params: params, result: result, seen: seen, started: make(chan (<-chan error), 1)}
	if connection.outbound == nil {
		done := make(chan error, 1)
		call.started <- done
		go func(ctx context.Context) { done <- connection.wire.CallObserved(ctx, method, params, result, seen) }(connection.identityCtx)
		return call, true
	}
	select {
	case connection.outbound <- call:
		return call, true
	case <-connection.wire.Done():
		return nil, false
	default:
		return nil, false
	}
}

func (call *workerCall) wait(connection *live) error {
	select {
	case done := <-call.started:
		return <-done
	case <-connection.wire.Done():
		select {
		case done := <-call.started:
			return <-done
		default:
			return rpc.ErrClosed
		}
	}
}
