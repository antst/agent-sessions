package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/antst/sessionbus/bus/internal/conn"
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
		return nil, errors.New("invalid sessionbus host")
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
	sweepSockets(config.SocketPath)
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

func sweepSockets(socket string) {
	paths := []string{socket}
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(socket), "lanes"))
	for _, entry := range entries {
		paths = append(paths, filepath.Join(filepath.Dir(socket), "lanes", entry.Name()))
	}
	for _, path := range paths {
		fd, err := net.Dial("unix", path)
		if err == nil {
			_ = fd.Close()
		} else if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(path)
		}
	}
}

func (d *Daemon) Done() <-chan struct{} { return d.done }

func (d *Daemon) accept() {
	defer close(d.acceptDone)
	for {
		fd, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.group.Add(1)
		s := newSession(d)
		s.wire = conn.Start(fd, s.inbox, &d.group)
		go func() { defer d.group.Done(); s.run() }()
	}
}

func (d *Daemon) Close() error {
	d.directory.mu.Lock()
	if d.directory.closing {
		d.directory.mu.Unlock()
		<-d.done
		return nil
	}
	d.directory.closing = true
	d.directory.mu.Unlock()
	_ = d.listener.Close()
	close(d.shutdown)
	<-d.acceptDone
	d.group.Wait()
	_ = os.Remove(d.config.SocketPath)
	close(d.done)
	return nil
}
