package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/antst/agent-sessions/bus/internal/rpc"
)

type Config struct {
	SocketPath string
	TablePath  string
	Host       string
	Products   []string
}

type live struct {
	wire           *rpc.Conn
	fd             net.Conn
	role           string
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
	child          *process
}

type source struct {
	live      *live
	sessionID string
	name      string
	product   string
	groups    []string
	private   string
	ctx       context.Context
}

type pendingRun struct {
	target *live
	caller *live
	done   chan struct{}
}

type reservation struct {
	product string
	hello   chan launchResult
}

type Daemon struct {
	config   Config
	host     string
	table    *table
	listener net.Listener

	mapsMutex    sync.Mutex
	connections  map[string]*live
	pending      map[string]*pendingRun
	rowLocks     map[string]*sync.Mutex
	reservations map[string]*reservation

	closeOnce  sync.Once
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
	for _, product := range config.Products {
		if !validHost(product) {
			return nil, errors.New("invalid advertised product")
		}
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
		connections: map[string]*live{}, pending: map[string]*pendingRun{}, rowLocks: map[string]*sync.Mutex{}, reservations: map[string]*reservation{},
		done: make(chan struct{}), acceptDone: make(chan struct{})}
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
		close(ready)
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
		connections := make([]*live, 0, len(d.connections))
		for _, connection := range d.connections {
			connections = append(connections, connection)
		}
		d.mapsMutex.Unlock()
		for _, connection := range connections {
			_ = connection.wire.Close()
			connection.child.killAndWait()
		}
		<-d.acceptDone
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
	d.mapsMutex.Lock()
	role, committed, id := connection.role, connection.committed, connection.sessionID
	d.mapsMutex.Unlock()
	if role == "peer" {
		d.removePeer(connection, id)
		return
	}
	if role != "worker" || !committed {
		return
	}
	value, ok := d.table.get(id)
	if !ok {
		connection.child.killAndWait()
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
	connection.child.killAndWait()
	lock.Unlock()
}

func (d *Daemon) removePeer(connection *live, id string) {
	d.mapsMutex.Lock()
	cancel := connection.cancelIdentity
	if d.connections[id] == connection {
		delete(d.connections, id)
		for target, pending := range d.pending {
			if pending.caller == connection || pending.target == connection {
				delete(d.pending, target)
			}
		}
	}
	d.mapsMutex.Unlock()
	if cancel != nil {
		cancel()
	}
}
