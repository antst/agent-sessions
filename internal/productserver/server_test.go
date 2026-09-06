package productserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/sessionbus/internal/procinfo"
	"github.com/antst/sessionbus/internal/productruntime"
)

type serverProcessFixture struct {
	done   chan struct{}
	once   sync.Once
	starts atomic.Int64
}

func (fixture *serverProcessFixture) Start(context.Context, productruntime.NativeCommand) (productruntime.OwnedProcessRef, error) {
	fixture.starts.Add(1)
	return productruntime.OwnedProcessRef{Process: procinfo.Identity{PID: 42}, ProcessGroup: 42}, nil
}

func (fixture *serverProcessFixture) Signal(context.Context, productruntime.OwnedProcessRef, productruntime.ProcessSignal) error {
	fixture.once.Do(func() { close(fixture.done) })
	return nil
}

func (fixture *serverProcessFixture) Wait(ctx context.Context, _ productruntime.OwnedProcessRef) (productruntime.ProcessExit, error) {
	select {
	case <-ctx.Done():
		return productruntime.ProcessExit{}, ctx.Err()
	case <-fixture.done:
		return productruntime.ProcessExit{}, nil
	}
}

func TestOwnedServerStartsProbesAndStopsOneProcess(t *testing.T) {
	auth, _ := NewMemoryAuth("X-Test", productruntime.NewSensitiveValue("ready"))
	native := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !auth.Authorize(request) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer native.Close()
	processes := &serverProcessFixture{done: make(chan struct{})}
	owned, err := StartOwnedServer(context.Background(), OwnedServerConfig{
		Command:        productruntime.NativeCommand{Path: "/bin/native"},
		Endpoint:       native.URL,
		Auth:           auth,
		Supervisor:     processes,
		StartupTimeout: time.Second,
		Ready: func(ctx context.Context, client *Client) error {
			_, requestErr := client.Do(ctx, Request{Method: http.MethodGet, Path: "/ready"})
			return requestErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if owned.Ref().Process.PID != 42 || owned.Client() == nil {
		t.Fatalf("owned server = %#v", owned)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := owned.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if exit, err := owned.Wait(ctx); err != nil || exit.ExitLike != 0 {
		t.Fatalf("wait = %#v, %v", exit, err)
	}
}

func TestOwnedServerBoundsEachReadinessAttempt(t *testing.T) {
	auth, _ := NewMemoryAuth("X-Test", productruntime.NewSensitiveValue("ready"))
	processes := &serverProcessFixture{done: make(chan struct{})}
	var attempts atomic.Int64
	owned, err := StartOwnedServer(context.Background(), OwnedServerConfig{
		Command:        productruntime.NativeCommand{Path: "/bin/native"},
		Endpoint:       "http://127.0.0.1:1",
		Auth:           auth,
		Supervisor:     processes,
		StartupTimeout: 2 * time.Second,
		ProbeInterval:  time.Millisecond,
		Ready: func(ctx context.Context, _ *Client) error {
			if attempts.Add(1) == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("readiness attempts = %d, want 2", attempts.Load())
	}
	if processes.starts.Load() != 1 {
		t.Fatalf("started processes = %d, want 1", processes.starts.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := owned.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
