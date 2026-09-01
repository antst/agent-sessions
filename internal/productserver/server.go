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
	ErrInvalidOwnedRef = errors.New("owned product server returned an invalid process reference")
)

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
	stopOnce        sync.Once
	stopDone        chan struct{}
	stopMu          sync.Mutex
	stopErr         error
}

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
		client.CloseIdleConnections()
		return nil, fmt.Errorf("start owned product server: %w", err)
	}
	if !validOwnedRef(ref) {
		client.CloseIdleConnections()
		return nil, ErrInvalidOwnedRef
	}
	server := &OwnedServer{
		ref: ref, client: client, supervisor: config.Supervisor,
		interruptGrace: interruptGrace, terminateGrace: terminateGrace,
		shutdownTimeout: shutdownTimeout, exitDone: make(chan struct{}), stopDone: make(chan struct{}),
	}
	go server.waitOnce()
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := server.awaitReady(startupCtx, probeInterval, config.Ready); err != nil {
		server.startStop()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), shutdownTimeout+interruptGrace+terminateGrace)
		defer cleanupCancel()
		select {
		case <-server.stopDone:
		case <-cleanupCtx.Done():
		}
		client.CloseIdleConnections()
		if errors.Is(err, ErrServerExited) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrServerNotReady
	}
	return server, nil
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

func (server *OwnedServer) awaitReady(ctx context.Context, interval time.Duration, ready ReadyFunc) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-server.exitDone:
			return ErrServerExited
		default:
		}
		if err := ready(ctx, server.client); err == nil {
			select {
			case <-server.exitDone:
				return ErrServerExited
			default:
				return nil
			}
		}
		select {
		case <-server.exitDone:
			return ErrServerExited
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
	server.startStop()
	select {
	case <-server.stopDone:
		server.stopMu.Lock()
		defer server.stopMu.Unlock()
		return server.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (server *OwnedServer) startStop() {
	server.stopOnce.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), server.shutdownTimeout)
			defer cancel()
			err := server.stop(ctx)
			server.client.CloseIdleConnections()
			server.stopMu.Lock()
			server.stopErr = err
			server.stopMu.Unlock()
			close(server.stopDone)
		}()
	})
}

func (server *OwnedServer) stop(ctx context.Context) error {
	select {
	case <-server.exitDone:
		_, err := server.Wait(context.Background())
		return err
	default:
	}
	var signalErrors []error
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
		if step.grace == 0 {
			select {
			case <-server.exitDone:
				_, waitErr := server.Wait(context.Background())
				return errors.Join(append(signalErrors, waitErr)...)
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
			_, waitErr := server.Wait(context.Background())
			return errors.Join(append(signalErrors, waitErr)...)
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
