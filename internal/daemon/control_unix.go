//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/antst/sessionbus/internal/pathidentity"
	"github.com/antst/sessionbus/internal/socketpath"
)

// ControlServer serves one fixed same-user local control endpoint.
type ControlServer struct {
	endpoint  string
	listener  *net.UnixListener
	lease     *os.File
	policy    *controlPolicy
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

// ControlCall is one already-admitted request whose response may arrive after
// a native process reports the fact needed to complete it.
type ControlCall struct {
	connection net.Conn
	requestID  string
	stopCancel func() bool
	closeOnce  sync.Once
}

type controlAdmissionContextKey struct{}

// AdmitControlCall acknowledges that a blocking request is fully registered.
// The caller may start the native process only after this same-call ack.
func AdmitControlCall(ctx context.Context) bool {
	admit, ok := ctx.Value(controlAdmissionContextKey{}).(func())
	if !ok {
		return false
	}
	admit()
	return true
}

// ControlEndpoint returns the fixed canonical endpoint for one state root.
func ControlEndpoint(stateRoot string) (string, error) {
	endpoint, err := pathidentity.FuturePath(filepath.Join(stateRoot, "run", "daemon.sock"))
	if err != nil {
		return "", fmt.Errorf("resolve control endpoint: %w", err)
	}
	if err := socketpath.Validate(endpoint); err != nil {
		return "", err
	}
	return endpoint, nil
}

// StartControlServer binds and starts one local control authority. A live or
// ambiguous pre-existing endpoint is never unlinked.
func StartControlServer(
	parent context.Context, stateRoot string, generation uint64, releaseIdentity string, handler ControlHandler,
) (*ControlServer, error) {
	if parent == nil {
		return nil, errors.New("control server context is nil")
	}
	if generation == 0 {
		return nil, errors.New("control server generation must be positive")
	}
	endpoint, lease, err := prepareControlEndpoint(stateRoot)
	if err != nil {
		return nil, err
	}
	releaseLease := true
	defer func() {
		if releaseLease {
			releaseControlLease(lease)
		}
	}()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("bind control endpoint without replacing existing authority: %w", err)
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("secure control endpoint: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	server := &ControlServer{
		endpoint: endpoint, listener: listener, lease: lease,
		policy: &controlPolicy{
			generation: generation, releaseIdentity: releaseIdentity, handler: handler,
			cache:    map[controlMutationCacheKey]cachedControlResponse{},
			inFlight: map[controlMutationCacheKey]*inFlightControlResponse{},
		},
		ctx: ctx, cancel: cancel,
	}
	releaseLease = false
	server.wg.Add(1)
	go server.accept()
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	return server, nil
}

func prepareControlEndpoint(stateRoot string) (string, *os.File, error) {
	endpoint, err := ControlEndpoint(stateRoot)
	if err != nil {
		return "", nil, err
	}
	runtimeRoot := filepath.Dir(endpoint)
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return "", nil, fmt.Errorf("create control runtime root: %w", err)
	}
	if err := os.Chmod(runtimeRoot, 0o700); err != nil { //nolint:gosec // private directories require owner traversal.
		return "", nil, fmt.Errorf("secure control runtime root: %w", err)
	}
	identity, err := pathidentity.ExistingNoFollow(runtimeRoot)
	if err != nil || identity.Kind != pathidentity.KindDirectory || identity.Mode.Perm() != 0o700 {
		return "", nil, errors.New("control runtime root is not a private real directory")
	}
	lease, err := acquireControlLease(runtimeRoot)
	if err != nil {
		return "", nil, err
	}
	if err := recoverStaleControlEndpoint(endpoint); err != nil {
		releaseControlLease(lease)
		return "", nil, err
	}
	return endpoint, lease, nil
}

// Endpoint returns the bound canonical socket path.
func (s *ControlServer) Endpoint() string { return s.endpoint }

// Close stops the listener and waits for accepted requests to finish.
func (s *ControlServer) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.closeErr = s.listener.Close()
		if errors.Is(s.closeErr, net.ErrClosed) {
			s.closeErr = nil
		}
		s.wg.Wait()
		releaseControlLease(s.lease)
	})
	return s.closeErr
}

func acquireControlLease(runtimeRoot string) (*os.File, error) {
	path, err := pathidentity.FuturePath(filepath.Join(runtimeRoot, "daemon.lock"))
	if err != nil {
		return nil, fmt.Errorf("resolve control lease: %w", err)
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open control lease: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open control lease returned no file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		_ = file.Close()
		return nil, errors.New("control lease is not a private real file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("existing authority owns the control lease")
		}
		return nil, fmt.Errorf("acquire control lease: %w", err)
	}
	return file, nil
}

func releaseControlLease(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func recoverStaleControlEndpoint(endpoint string) error {
	before, err := os.Lstat(endpoint)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing control endpoint: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || before.Mode()&os.ModeSocket == 0 {
		return errors.New("existing control endpoint is not a real Unix socket")
	}
	connection, dialErr := net.DialTimeout("unix", endpoint, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return errors.New("existing authority is serving the control endpoint")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("existing control endpoint liveness is ambiguous: %w", dialErr)
	}
	after, err := os.Lstat(endpoint)
	if err != nil || !os.SameFile(before, after) || after.Mode()&os.ModeSocket == 0 {
		return errors.New("existing control endpoint changed during stale recovery")
	}
	if err := os.Remove(endpoint); err != nil {
		return fmt.Errorf("remove stale control endpoint: %w", err)
	}
	return nil
}

func (s *ControlServer) accept() {
	defer s.wg.Done()
	for {
		connection, err := s.listener.AcceptUnix()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serveConnection(connection)
	}
}

func (s *ControlServer) serveConnection(connection *net.UnixConn) {
	defer s.wg.Done()
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	var request ControlRequest
	if err := readControlFrame(connection, &request); err != nil {
		return
	}
	// Bound admission of the request frame, but do not impose that short
	// deadline on a native model turn. Lane `run` is intentionally synchronous
	// and real provider turns routinely exceed thirty seconds; the client
	// context remains the authority for cancelling the call.
	_ = connection.SetDeadline(time.Time{})
	requestCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		var extra [1]byte
		if _, err := connection.Read(extra[:]); err != nil {
			cancel()
		}
	}()
	if request.WaitAdmission {
		admitted := make(chan struct{})
		var admitOnce sync.Once
		requestCtx = context.WithValue(requestCtx, controlAdmissionContextKey{}, func() {
			admitOnce.Do(func() { close(admitted) })
		})
		completed := make(chan ControlResponse, 1)
		go func() { completed <- s.policy.handle(requestCtx, request) }()
		select {
		case response := <-completed:
			response.Generation = s.policy.generation
			response.ReleaseIdentity = s.policy.releaseIdentity
			_ = writeControlFrame(connection, response)
			return
		case <-admitted:
			if err := writeControlFrame(connection, ControlResponse{
				ID: request.ID, Generation: s.policy.generation, ReleaseIdentity: s.policy.releaseIdentity, OK: true, Admitted: true,
			}); err != nil {
				return
			}
		case <-requestCtx.Done():
			return
		}
		response := <-completed
		response.Generation = s.policy.generation
		response.ReleaseIdentity = s.policy.releaseIdentity
		_ = writeControlFrame(connection, response)
		return
	}
	response := s.policy.handle(requestCtx, request)
	response.Generation = s.policy.generation
	response.ReleaseIdentity = s.policy.releaseIdentity
	_ = writeControlFrame(connection, response)
}

// BeginControlCall writes one request before returning. Await completes that
// same request; closing the caller or cancelling its context cancels the
// server-side handler through connection EOF.
func BeginControlCall(ctx context.Context, endpoint string, request ControlRequest) (*ControlCall, error) {
	if ctx == nil {
		return nil, errors.New("control call context is nil")
	}
	if err := socketpath.Validate(endpoint); err != nil {
		return nil, err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, fmt.Errorf("dial daemon control endpoint: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := writeControlFrame(connection, request); err != nil {
		_ = connection.Close()
		return nil, err
	}
	call := &ControlCall{connection: connection, requestID: request.ID}
	call.stopCancel = context.AfterFunc(ctx, func() { _ = call.Close() })
	if request.WaitAdmission {
		var response ControlResponse
		if err := readControlFrame(connection, &response); err != nil {
			_ = call.Close()
			return nil, err
		}
		if response.ID != request.ID {
			_ = call.Close()
			return nil, errors.New("daemon control admission correlation mismatch")
		}
		if response.Error != nil {
			_ = call.Close()
			return nil, errors.New(response.Error.Message)
		}
		if !response.OK || !response.Admitted {
			_ = call.Close()
			return nil, errors.New("daemon did not admit the blocking control call")
		}
	}
	return call, nil
}

// Await reads the response to the request written by BeginControlCall.
func (c *ControlCall) Await() (ControlResponse, error) {
	if c == nil || c.connection == nil {
		return ControlResponse{}, errors.New("control call is unavailable")
	}
	defer func() { _ = c.Close() }()
	var response ControlResponse
	if err := readControlFrame(c.connection, &response); err != nil {
		return ControlResponse{}, err
	}
	if response.ID != c.requestID {
		return ControlResponse{}, errors.New("daemon control response correlation mismatch")
	}
	return response, nil
}

// Close abandons the one in-flight call.
func (c *ControlCall) Close() error {
	if c == nil {
		return nil
	}
	var err error
	c.closeOnce.Do(func() {
		if c.stopCancel != nil {
			c.stopCancel()
		}
		if c.connection != nil {
			err = c.connection.Close()
		}
	})
	return err
}

// CallControl performs one bounded request against the running daemon.
func CallControl(ctx context.Context, endpoint string, request ControlRequest) (ControlResponse, error) {
	if ctx == nil {
		return ControlResponse{}, errors.New("control call context is nil")
	}
	if err := socketpath.Validate(endpoint); err != nil {
		return ControlResponse{}, err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("dial daemon control endpoint: %w", err)
	}
	defer func() { _ = connection.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := writeControlFrame(connection, request); err != nil {
		return ControlResponse{}, err
	}
	var response ControlResponse
	if err := readControlFrame(connection, &response); err != nil {
		return ControlResponse{}, err
	}
	if response.ID != request.ID {
		return ControlResponse{}, errors.New("daemon control response correlation mismatch")
	}
	return response, nil
}
