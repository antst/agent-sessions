package federator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ShadowOptions configures one supervised local proxy for a remote peer.
type ShadowOptions struct {
	Listen      string
	Control     string
	TargetID    string
	OwnerPID    int
	RegistryDir string
	Logger      *log.Logger
}

// RunShadow exposes one remote peer through a native local Unix socket.
func RunShadow(ctx context.Context, options ShadowOptions) error {
	if options.Listen == "" || options.Control == "" || options.TargetID == "" {
		return errors.New("shadow requires listen, control, and target")
	}
	if err := ensureDir(filepath.Dir(options.Listen)); err != nil {
		return err
	}
	// Serialize ownership of the deterministic socket path across agent
	// generations. A replacement agent may start before an old shadow notices
	// that its owner died; without this lock, the old shadow can unlink the
	// replacement listener during its deferred cleanup.
	socketLock, err := os.OpenFile(options.Listen+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(socketLock.Fd()), syscall.LOCK_EX); err != nil {
		_ = socketLock.Close()
		return err
	}
	defer func() {
		_ = syscall.Flock(int(socketLock.Fd()), syscall.LOCK_UN)
		_ = socketLock.Close()
	}()
	_ = os.Remove(options.Listen)
	listener, err := net.Listen("unix", options.Listen)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(options.Listen)
		if options.RegistryDir != "" {
			_ = os.Remove(filepath.Join(options.RegistryDir, fmt.Sprintf("%d.json", os.Getpid())))
		}
	}()
	logger := defaultLogger(options.Logger)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = listener.Close()
				return
			case <-ticker.C:
				if options.OwnerPID > 0 && !processLive(options.OwnerPID) {
					_ = listener.Close()
					return
				}
			}
		}
	}()
	return serveShadow(ctx, listener, options, logger)
}

func serveShadow(ctx context.Context, listener net.Listener, options ShadowOptions, logger *log.Logger) error {
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || (options.OwnerPID > 0 && !processLive(options.OwnerPID)) {
				//nolint:nilerr // Closing the listener is the expected clean shutdown signal.
				return nil
			}
			return acceptErr
		}
		go handleShadowConnection(conn, options.Control, options.TargetID, logger)
	}
}

func handleShadowConnection(conn net.Conn, control, targetID string, logger *log.Logger) {
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxWireBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if !json.Valid(line) {
			continue
		}
		controlConn, err := net.DialTimeout("unix", control, 2*time.Second)
		if err != nil {
			logger.Printf("deliver to agent failed for %s: %v", targetID, err)
			continue
		}
		wire := newWireConn(controlConn)
		err = wire.Send(Message{Type: "shadow_deliver", TargetID: targetID, Frame: line})
		_ = controlConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var response Message
		decodeErr := json.NewDecoder(controlConn).Decode(&response)
		_ = controlConn.Close()
		if err != nil || decodeErr != nil || response.Type != "accepted" {
			logger.Printf("agent rejected delivery for %s", targetID)
		}
	}
}

func stopProcess(process *os.Process, timeout time.Duration) {
	if process == nil {
		return
	}
	_ = process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if process.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = process.Kill()
}
