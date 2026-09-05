package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/antst/agent-sessions/bus/internal/conn"
)

type Config struct {
	SocketPath string
	TablePath  string
	Host       string
	Products   []string
}

type Daemon struct {
	config     Config
	host       string
	table      *table
	directory  *directory
	listener   net.Listener
	closing    atomic.Bool
	shutdown   chan struct{}
	done       chan struct{}
	acceptDone chan struct{}
	group      sync.WaitGroup
}

func Start(config Config) (*Daemon, error) {
	if config.Host == "" {
		config.Host = "local"
	}
	if !validHost(config.Host) {
		return nil, errors.New("invalid agentbus host")
	}
	seen := map[string]bool{}
	for _, product := range config.Products {
		if !validHost(product) {
			return nil, errors.New("invalid advertised product")
		}
		if seen[product] {
			return nil, errors.New("duplicate advertised product")
		}
		seen[product] = true
	}
	if len(config.Products) == 0 {
		config.Products = nil
	}
	store, rows, err := openTable(config.TablePath)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(config.SocketPath), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(config.SocketPath)
	listener, err := net.Listen("unix", config.SocketPath)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(config.SocketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	d := &Daemon{config: config, host: config.Host, table: store, listener: listener,
		shutdown: make(chan struct{}), done: make(chan struct{}), acceptDone: make(chan struct{})}
	d.directory = newDirectory(d, rows)
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
		if d.closing.Load() {
			_ = fd.Close()
			continue
		}
		d.group.Add(1)
		s := newSession(d)
		s.wire = conn.Start(fd, s.inbox, &d.group)
		go func() { defer d.group.Done(); s.run() }()
	}
}

func (d *Daemon) Close() error {
	if !d.closing.CompareAndSwap(false, true) {
		<-d.done
		return nil
	}
	_ = d.listener.Close()
	close(d.shutdown)
	<-d.acceptDone
	d.directory.mu.Lock()
	d.directory.mu.Unlock()
	d.group.Wait()
	_ = os.Remove(d.config.SocketPath)
	close(d.done)
	return nil
}
