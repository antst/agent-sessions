package productserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
)

var (
	ErrServerClosed    = errors.New("product server is closed")
	ErrEndpointInUse   = errors.New("product server endpoint is already in use")
	ErrServerNotReady  = errors.New("owned product server did not become ready")
	ErrServerExited    = errors.New("owned product server exited before readiness")
	ErrServerWait      = errors.New("owned product server exit observation failed")
	ErrInvalidOwnedRef = errors.New("owned product server returned an invalid process reference")
	ErrCleanupDebt     = errors.New("owned product server cleanup is incomplete")
)

// CleanupDebtError preserves the only exact process reference when cleanup
// cannot authoritatively prove exit. Its text and Go formatting omit the
// underlying failure; errors.Is/errors.As retain the machine-readable causes.
type CleanupDebtError struct {
	Ref   productruntime.OwnedProcessRef
	cause error
}

func (*CleanupDebtError) Error() string    { return ErrCleanupDebt.Error() }
func (*CleanupDebtError) GoString() string { return "productserver.CleanupDebtError{[REDACTED]}" }
func (debt *CleanupDebtError) Unwrap() []error {
	causes := []error{ErrCleanupDebt}
	if debt != nil && debt.cause != nil {
		causes = append(causes, debt.cause)
	}
	return causes
}

type serverWaitError struct{ cause error }

func (serverWaitError) Error() string { return ErrServerWait.Error() }
func (failure serverWaitError) Unwrap() []error {
	return []error{ErrServerWait, failure.cause}
}

type cleanupOutcomeError struct{ cause error }

func (cleanupOutcomeError) Error() string { return "owned product server cleanup reported an error" }
func (failure cleanupOutcomeError) Unwrap() error {
	return failure.cause
}

const (
	defaultServerReadHeaderTimeout = 10 * time.Second
	defaultServerIdleTimeout       = 30 * time.Second
	defaultStartupTimeout          = 30 * time.Second
	defaultProbeInterval           = 25 * time.Millisecond
	defaultInterruptGrace          = 2 * time.Second
	defaultTerminateGrace          = 2 * time.Second
	defaultShutdownTimeout         = 10 * time.Second
	endpointProbeTimeout           = 150 * time.Millisecond
)

// LocalServerConfig configures a small authenticated loopback HTTP server.
// The wrapper authenticates and fully bounds request decompression before the
// product-neutral handler receives a body.
type LocalServerConfig struct {
	Address           string
	Auth              MemoryAuth
	Limits            Limits
	Handler           http.Handler
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
}

// LocalServer owns only the listener it created.
type LocalServer struct {
	listener  net.Listener
	server    *http.Server
	endpoint  string
	done      chan struct{}
	mu        sync.Mutex
	serveErr  error
	closeErr  error
	closeOnce sync.Once
}

func StartLocalServer(config LocalServerConfig) (*LocalServer, error) {
	if !config.Auth.valid() {
		return nil, ErrInvalidAuth
	}
	if config.Handler == nil {
		return nil, errors.New("product server handler is required")
	}
	if err := validateListenAddress(config.Address); err != nil {
		return nil, err
	}
	limits, err := config.Limits.normalized()
	if err != nil {
		return nil, err
	}
	readHeaderTimeout := config.ReadHeaderTimeout
	if readHeaderTimeout == 0 {
		readHeaderTimeout = defaultServerReadHeaderTimeout
	}
	idleTimeout := config.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultServerIdleTimeout
	}
	if readHeaderTimeout < 0 || idleTimeout < 0 {
		return nil, ErrInvalidLimits
	}
	listener, err := net.Listen("tcp", config.Address)
	if err != nil {
		return nil, fmt.Errorf("listen for product server: %w", err)
	}
	local := &LocalServer{
		listener: listener,
		endpoint: "http://" + listener.Addr().String(),
		done:     make(chan struct{}),
	}
	local.server = &http.Server{
		Handler:           boundedAuthenticatedHandler(config.Auth, limits, config.Handler),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    int(limits.MaxHeaderBytes),
	}
	go func() {
		err := local.server.Serve(listener)
		local.mu.Lock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			local.serveErr = err
		}
		local.mu.Unlock()
		close(local.done)
	}()
	return local, nil
}

func validateListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return ErrNonLoopback
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return ErrNonLoopback
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return ErrNonLoopback
	}
	return nil
}

func boundedAuthenticatedHandler(auth MemoryAuth, limits Limits, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		if request.URL == nil || request.URL.IsAbs() || request.URL.Host != "" {
			http.Error(response, "invalid request target", http.StatusBadRequest)
			return
		}
		if !auth.Authorize(request) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		deleteHeaderFold(request.Header, auth.header)
		if request.ContentLength > limits.MaxRequestWireBytes {
			http.Error(response, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		body, status, err := readServerRequest(request, limits)
		_ = request.Body.Close()
		if err != nil {
			http.Error(response, http.StatusText(status), status)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		request.Header.Del("Content-Encoding")
		next.ServeHTTP(response, request)
	})
}

func readServerRequest(request *http.Request, limits Limits) ([]byte, int, error) {
	if request.Body == nil {
		return nil, 0, nil
	}
	wire, err := readBounded(request.Body, limits.MaxRequestWireBytes, ErrRequestTooLarge)
	if err != nil {
		return nil, http.StatusRequestEntityTooLarge, err
	}
	switch normalizedEncoding(request.Header) {
	case "", "identity":
		if int64(len(wire)) > limits.MaxRequestBodyBytes {
			return nil, http.StatusRequestEntityTooLarge, ErrRequestTooLarge
		}
		return wire, 0, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil, http.StatusBadRequest, ErrInvalidResponse
		}
		body, readErr := readBounded(reader, limits.MaxRequestBodyBytes, ErrRequestTooLarge)
		closeErr := reader.Close()
		if errors.Is(readErr, ErrRequestTooLarge) {
			return nil, http.StatusRequestEntityTooLarge, readErr
		}
		if readErr != nil || closeErr != nil {
			return nil, http.StatusBadRequest, ErrInvalidResponse
		}
		return body, 0, nil
	default:
		return nil, http.StatusUnsupportedMediaType, ErrUnsupportedEncoding
	}
}

func (server *LocalServer) Endpoint() string {
	if server == nil {
		return ""
	}
	return server.endpoint
}

func (server *LocalServer) Done() <-chan struct{} {
	if server == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return server.done
}

func (server *LocalServer) Err() error {
	if server == nil {
		return ErrServerClosed
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.serveErr
}

func (server *LocalServer) Close(ctx context.Context) error {
	if server == nil {
		return nil
	}
	if ctx == nil {
		return ErrServerClosed
	}
	server.closeOnce.Do(func() {
		server.closeErr = server.server.Shutdown(ctx)
		if server.closeErr != nil {
			_ = server.server.Close()
		}
	})
	select {
	case <-server.done:
		if server.closeErr != nil {
			return server.closeErr
		}
		return server.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReadyFunc performs one product-owned readiness observation through the safe
// client. It keeps route and response semantics out of this package.
type ReadyFunc func(context.Context, *Client) error

type OwnedServerConfig struct {
	Command         productruntime.NativeCommand
	Endpoint        string
	Auth            MemoryAuth
	Limits          Limits
	Supervisor      productruntime.OwnedProcessSupervisor
	Ready           ReadyFunc
	StartupTimeout  time.Duration
	ProbeInterval   time.Duration
	InterruptGrace  time.Duration
	TerminateGrace  time.Duration
	ShutdownTimeout time.Duration
}

type ownedExit struct {
	exit productruntime.ProcessExit
	err  error
}

type stopAttempt struct {
	done chan struct{}
	err  error
}

// OwnedServer supervises exactly the ephemeral process reference returned by
// OwnedProcessSupervisor.Start. It never signals a PID or process group derived
// from an endpoint or process-table search.
type OwnedServer struct {
	ref             productruntime.OwnedProcessRef
	client          *Client
	supervisor      productruntime.OwnedProcessSupervisor
	interruptGrace  time.Duration
	terminateGrace  time.Duration
	shutdownTimeout time.Duration
	exitDone        chan struct{}
	exitMu          sync.Mutex
	exitResult      ownedExit
	stopMu          sync.Mutex
	activeStop      *stopAttempt
}

// StartOwnedServer starts and observes one exact owned server. When post-start
// cleanup cannot prove exit it deliberately returns both a non-nil server and
// an error matching ErrCleanupDebt; callers must retain the server or the
// CleanupDebtError.Ref and retry exact cleanup.
func StartOwnedServer(ctx context.Context, config OwnedServerConfig) (*OwnedServer, error) {
	if ctx == nil || config.Supervisor == nil || config.Ready == nil || config.Command.Path == "" {
		return nil, ErrServerNotReady
	}
	client, err := NewClient(ClientConfig{Endpoint: config.Endpoint, Auth: config.Auth, Limits: config.Limits})
	if err != nil {
		return nil, err
	}
	if endpointInUse(client.dialAddr) {
		client.CloseIdleConnections()
		return nil, ErrEndpointInUse
	}
	startupTimeout := durationOrDefault(config.StartupTimeout, defaultStartupTimeout)
	probeInterval := durationOrDefault(config.ProbeInterval, defaultProbeInterval)
	interruptGrace := durationOrDefault(config.InterruptGrace, defaultInterruptGrace)
	terminateGrace := durationOrDefault(config.TerminateGrace, defaultTerminateGrace)
	shutdownTimeout := durationOrDefault(config.ShutdownTimeout, defaultShutdownTimeout)
	if startupTimeout <= 0 || probeInterval <= 0 || interruptGrace <= 0 || terminateGrace <= 0 ||
		shutdownTimeout <= interruptGrace+terminateGrace {
		client.CloseIdleConnections()
		return nil, ErrInvalidLimits
	}
	ref, err := config.Supervisor.Start(ctx, config.Command)
	if err != nil {
		startFailure := fmt.Errorf("start owned product server: %w", err)
		if ref == (productruntime.OwnedProcessRef{}) {
			client.CloseIdleConnections()
			return nil, startFailure
		}
		server := newOwnedServer(ref, client, config.Supervisor, interruptGrace, terminateGrace, shutdownTimeout)
		return server.failAfterStart(startFailure)
	}
	server := newOwnedServer(ref, client, config.Supervisor, interruptGrace, terminateGrace, shutdownTimeout)
	if !validOwnedRef(ref) {
		return server.failAfterStart(ErrInvalidOwnedRef)
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := server.awaitReady(startupCtx, probeInterval, config.Ready); err != nil {
		failure := error(ErrServerNotReady)
		if errors.Is(err, ErrServerExited) || errors.Is(err, ErrServerWait) {
			failure = err
		}
		if ctx.Err() != nil {
			failure = ctx.Err()
		}
		return server.failAfterStart(failure)
	}
	return server, nil
}

func newOwnedServer(
	ref productruntime.OwnedProcessRef,
	client *Client,
	supervisor productruntime.OwnedProcessSupervisor,
	interruptGrace time.Duration,
	terminateGrace time.Duration,
	shutdownTimeout time.Duration,
) *OwnedServer {
	server := &OwnedServer{
		ref: ref, client: client, supervisor: supervisor,
		interruptGrace: interruptGrace, terminateGrace: terminateGrace,
		shutdownTimeout: shutdownTimeout, exitDone: make(chan struct{}),
	}
	go server.waitOnce()
	return server
}

func (server *OwnedServer) failAfterStart(failure error) (*OwnedServer, error) {
	attempt := server.startStop()
	cleanupTimer := time.NewTimer(server.shutdownTimeout + time.Second)
	defer cleanupTimer.Stop()
	var cleanupErr error
	select {
	case <-attempt.done:
		cleanupErr = attempt.err
	case <-cleanupTimer.C:
		cleanupErr = context.DeadlineExceeded
	}
	if server.authoritativeExitObserved() {
		server.client.CloseIdleConnections()
		if cleanupErr != nil {
			return nil, errors.Join(failure, cleanupOutcomeError{cause: cleanupErr})
		}
		return nil, failure
	}
	debt := &CleanupDebtError{Ref: server.ref, cause: cleanupErr}
	return server, errors.Join(failure, debt)
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

func validOwnedRef(ref productruntime.OwnedProcessRef) bool {
	return ref.Process.PID > 1 && ref.Process.Start != "" && ref.Process.StrongStart != "" && ref.ProcessGroup > 1
}

func endpointInUse(address string) bool {
	connection, err := net.DialTimeout("tcp", address, endpointProbeTimeout)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func (server *OwnedServer) waitOnce() {
	exit, err := server.supervisor.Wait(context.Background(), server.ref)
	server.exitMu.Lock()
	server.exitResult = ownedExit{exit: exit, err: err}
	server.exitMu.Unlock()
	close(server.exitDone)
}

func (server *OwnedServer) completedWait() (ownedExit, bool) {
	select {
	case <-server.exitDone:
		server.exitMu.Lock()
		defer server.exitMu.Unlock()
		return server.exitResult, true
	default:
		return ownedExit{}, false
	}
}

func (server *OwnedServer) authoritativeExitObserved() bool {
	result, completed := server.completedWait()
	return completed && result.err == nil
}

func (server *OwnedServer) waitCompletionError() error {
	result, completed := server.completedWait()
	if !completed {
		return nil
	}
	if result.err != nil {
		return serverWaitError{cause: result.err}
	}
	return ErrServerExited
}

func (server *OwnedServer) awaitReady(ctx context.Context, interval time.Duration, ready ReadyFunc) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-server.exitDone:
			return server.waitCompletionError()
		default:
		}
		if err := ready(ctx, server.client); err == nil {
			select {
			case <-server.exitDone:
				return server.waitCompletionError()
			default:
				return nil
			}
		}
		select {
		case <-server.exitDone:
			return server.waitCompletionError()
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (server *OwnedServer) Ref() productruntime.OwnedProcessRef {
	if server == nil {
		return productruntime.OwnedProcessRef{}
	}
	return server.ref
}

func (server *OwnedServer) Client() *Client {
	if server == nil {
		return nil
	}
	return server.client
}

func (server *OwnedServer) Wait(ctx context.Context) (productruntime.ProcessExit, error) {
	if server == nil {
		return productruntime.ProcessExit{}, ErrServerClosed
	}
	if ctx == nil {
		return productruntime.ProcessExit{}, ErrServerClosed
	}
	select {
	case <-server.exitDone:
		server.exitMu.Lock()
		defer server.exitMu.Unlock()
		return server.exitResult.exit, server.exitResult.err
	case <-ctx.Done():
		return productruntime.ProcessExit{}, ctx.Err()
	}
}

func (server *OwnedServer) Close(ctx context.Context) error {
	if server == nil {
		return nil
	}
	if ctx == nil {
		return ErrServerClosed
	}
	attempt := server.startStop()
	select {
	case <-attempt.done:
		if !server.authoritativeExitObserved() {
			return &CleanupDebtError{Ref: server.ref, cause: attempt.err}
		}
		return attempt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (server *OwnedServer) startStop() *stopAttempt {
	server.stopMu.Lock()
	if server.activeStop != nil {
		attempt := server.activeStop
		server.stopMu.Unlock()
		return attempt
	}
	attempt := &stopAttempt{done: make(chan struct{})}
	server.activeStop = attempt
	server.stopMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), server.shutdownTimeout)
		defer cancel()
		attempt.err = server.stop(ctx)
		if server.authoritativeExitObserved() {
			server.client.CloseIdleConnections()
		}
		server.stopMu.Lock()
		if server.activeStop == attempt {
			server.activeStop = nil
		}
		close(attempt.done)
		server.stopMu.Unlock()
	}()
	return attempt
}

func (server *OwnedServer) stop(ctx context.Context) error {
	if result, completed := server.completedWait(); completed && result.err == nil {
		return nil
	}
	var signalErrors []error
	waitFailed := false
	if result, completed := server.completedWait(); completed && result.err != nil {
		signalErrors = append(signalErrors, serverWaitError{cause: result.err})
		waitFailed = true
	}
	sequence := []struct {
		signal productruntime.ProcessSignal
		grace  time.Duration
	}{
		{signal: productruntime.ProcessInterrupt, grace: server.interruptGrace},
		{signal: productruntime.ProcessTerminate, grace: server.terminateGrace},
		{signal: productruntime.ProcessKill},
	}
	for _, step := range sequence {
		if err := server.supervisor.Signal(ctx, server.ref, step.signal); err != nil {
			signalErrors = append(signalErrors, err)
		}
		if waitFailed {
			if step.grace == 0 {
				return errors.Join(signalErrors...)
			}
			timer := time.NewTimer(step.grace)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return errors.Join(append(signalErrors, ctx.Err())...)
			}
			continue
		}
		if step.grace == 0 {
			select {
			case <-server.exitDone:
				result, _ := server.completedWait()
				if result.err == nil {
					return errors.Join(signalErrors...)
				}
				return errors.Join(append(signalErrors, serverWaitError{cause: result.err})...)
			case <-ctx.Done():
				return errors.Join(append(signalErrors, ctx.Err())...)
			}
		}
		timer := time.NewTimer(step.grace)
		select {
		case <-server.exitDone:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			result, _ := server.completedWait()
			if result.err == nil {
				return errors.Join(signalErrors...)
			}
			signalErrors = append(signalErrors, serverWaitError{cause: result.err})
			waitFailed = true
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return errors.Join(append(signalErrors, ctx.Err())...)
		}
	}
	return errors.Join(signalErrors...)
}
