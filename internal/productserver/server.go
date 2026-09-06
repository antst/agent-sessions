package productserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/antst/sessionbus/internal/productruntime"
)

var (
	ErrServerClosed   = errors.New("product server is closed")
	ErrServerNotReady = errors.New("owned product server did not become ready")
	ErrServerExited   = errors.New("owned product server exited before readiness")
)

const (
	defaultStartupTimeout      = 30 * time.Second
	defaultProbeInterval       = 25 * time.Millisecond
	defaultReadyAttemptTimeout = 500 * time.Millisecond
)

type ReadyFunc func(context.Context, *Client) error

// OwnedServerConfig retains the adapter-facing configuration shape. Shutdown
// retry/debt fields are ignored because cleanup is now one synchronous call.
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

type OwnedServer struct {
	ref        productruntime.OwnedProcessRef
	client     *Client
	supervisor productruntime.OwnedProcessSupervisor
	done       chan struct{}
	exit       ownedExit

	closeOnce sync.Once
	closeErr  error
}

func StartOwnedServer(ctx context.Context, config OwnedServerConfig) (*OwnedServer, error) {
	if ctx == nil || config.Supervisor == nil || config.Ready == nil || config.Command.Path == "" {
		return nil, ErrServerNotReady
	}
	client, err := NewClient(ClientConfig{Endpoint: config.Endpoint, Auth: config.Auth, Limits: config.Limits})
	if err != nil {
		return nil, err
	}
	ref, err := config.Supervisor.Start(ctx, config.Command)
	if err != nil {
		client.CloseIdleConnections()
		return nil, fmt.Errorf("start owned product server: %w", err)
	}
	server := &OwnedServer{
		ref: ref, client: client, supervisor: config.Supervisor, done: make(chan struct{}),
	}
	go func() {
		server.exit.exit, server.exit.err = config.Supervisor.Wait(context.Background(), ref)
		close(server.done)
	}()

	startupTimeout := config.StartupTimeout
	if startupTimeout == 0 {
		startupTimeout = defaultStartupTimeout
	}
	probeInterval := config.ProbeInterval
	if probeInterval == 0 {
		probeInterval = defaultProbeInterval
	}
	if startupTimeout <= 0 || probeInterval <= 0 {
		_ = server.Close(context.Background())
		return nil, ErrInvalidLimits
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		attemptCtx, cancelAttempt := context.WithTimeout(startupCtx, defaultReadyAttemptTimeout)
		readyErr := config.Ready(attemptCtx, client)
		cancelAttempt()
		if readyErr == nil {
			return server, nil
		}
		select {
		case <-server.done:
			client.CloseIdleConnections()
			return nil, ErrServerExited
		case <-startupCtx.Done():
			closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
			_ = server.Close(closeCtx)
			closeCancel()
			return nil, errors.Join(ErrServerNotReady, startupCtx.Err())
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
	if server == nil || ctx == nil {
		return productruntime.ProcessExit{}, ErrServerClosed
	}
	select {
	case <-ctx.Done():
		return productruntime.ProcessExit{}, ctx.Err()
	case <-server.done:
		return server.exit.exit, server.exit.err
	}
}

// Close asks the child to terminate, waits for it, and kills it if the caller's
// deadline expires. There is no retained cleanup debt.
func (server *OwnedServer) Close(ctx context.Context) error {
	if server == nil {
		return nil
	}
	if ctx == nil {
		return ErrServerClosed
	}
	server.closeOnce.Do(func() {
		_ = server.supervisor.Signal(ctx, server.ref, productruntime.ProcessTerminate)
		select {
		case <-server.done:
			server.closeErr = server.exit.err
		case <-ctx.Done():
			_ = server.supervisor.Signal(context.Background(), server.ref, productruntime.ProcessKill)
			server.closeErr = ctx.Err()
		}
		server.client.CloseIdleConnections()
	})
	return server.closeErr
}
