package productserver

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestLocalServerRequiresLiteralLoopbackMemoryAuthAndBoundedBodies(t *testing.T) {
	auth := testAuth(t)
	var calls atomic.Int32
	server, err := StartLocalServer(LocalServerConfig{
		Address: "127.0.0.1:0",
		Auth:    auth,
		Limits: Limits{
			MaxRequestWireBytes: 64,
			MaxRequestBodyBytes: 8,
		},
		Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if got := request.Header.Get("Authorization"); got != "" {
				t.Errorf("handler received memory credential %q", got)
			}
			calls.Add(1)
			body, _ := io.ReadAll(request.Body)
			_, _ = response.Write(body)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })

	plain := &http.Client{}
	response, err := plain.Post(server.Endpoint(), "text/plain", bytes.NewReader([]byte("ok")))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthenticated status/calls = %d/%d", response.StatusCode, calls.Load())
	}

	client := newTestClient(t, server.Endpoint(), auth, Limits{})
	got, err := client.Do(context.Background(), Request{Method: http.MethodPost, Path: "/", Body: []byte("12345678")})
	if err != nil || string(got.Body) != "12345678" {
		t.Fatalf("bounded request = %#v, %v", got, err)
	}

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write(bytes.Repeat([]byte("x"), 32))
	_ = writer.Close()
	request, _ := http.NewRequest(http.MethodPost, server.Endpoint(), bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	auth.Apply(request.Header)
	response, err = plain.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge || calls.Load() != 1 {
		t.Fatalf("gzip bomb status/calls = %d/%d", response.StatusCode, calls.Load())
	}

	request, _ = http.NewRequest(http.MethodGet, server.Endpoint(), nil)
	auth.Apply(request.Header)
	request.Header["authorization"] = []string{"Bearer duplicate"}
	response, err = plain.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || calls.Load() != 1 {
		t.Fatalf("duplicate auth status/calls = %d/%d", response.StatusCode, calls.Load())
	}

	address := strings.TrimPrefix(server.Endpoint(), "http://")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(connection, "GET http://example.com/escape HTTP/1.1\r\nHost: example.com\r\nAuthorization: %s\r\nConnection: close\r\n\r\n", auth.value.Reveal())
	rawResponse, err := http.ReadResponse(bufio.NewReader(connection), nil)
	_ = connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	_ = rawResponse.Body.Close()
	if rawResponse.StatusCode != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("absolute-form status/calls = %d/%d", rawResponse.StatusCode, calls.Load())
	}

	if _, err := StartLocalServer(LocalServerConfig{Address: "localhost:0", Auth: auth, Handler: http.NotFoundHandler()}); !errors.Is(err, ErrNonLoopback) {
		t.Fatalf("hostname listener error = %v, want ErrNonLoopback", err)
	}
}

func TestOwnedServerReportsEarlyExitWithoutSignalingAgain(t *testing.T) {
	auth := testAuth(t)
	ref := productruntime.OwnedProcessRef{
		Process:      procinfo.Identity{PID: 6262, Start: "start", StrongStart: "strong"},
		ProcessGroup: 6262,
	}
	supervisor := newFakeOwnedSupervisor(ref)
	close(supervisor.exit)
	_, err := StartOwnedServer(context.Background(), OwnedServerConfig{
		Command:         productruntime.NativeCommand{Path: "/native/server"},
		Endpoint:        unusedLoopbackEndpoint(t),
		Auth:            auth,
		Supervisor:      supervisor,
		StartupTimeout:  time.Second,
		ProbeInterval:   time.Millisecond,
		InterruptGrace:  time.Millisecond,
		TerminateGrace:  time.Millisecond,
		ShutdownTimeout: time.Second,
		Ready:           func(context.Context, *Client) error { return errors.New("not ready") },
	})
	if !errors.Is(err, ErrServerExited) {
		t.Fatalf("early exit error = %v, want ErrServerExited", err)
	}
	if got := supervisor.signalSnapshot(); len(got) != 0 {
		t.Fatalf("already exited server was signaled: %#v", got)
	}
	if got := supervisor.waits.Load(); got != 1 {
		t.Fatalf("Wait calls = %d, want 1", got)
	}
}

func TestOwnedServerReadinessAndCloseUseOnlyExactOwnedReference(t *testing.T) {
	auth := testAuth(t)
	ref := productruntime.OwnedProcessRef{
		Process:      procinfo.Identity{PID: 4242, Start: "start", StrongStart: "strong"},
		ProcessGroup: 4242,
	}
	supervisor := newFakeOwnedSupervisor(ref)
	supervisor.exitOn = productruntime.ProcessTerminate
	var probes atomic.Int32
	command := productruntime.NativeCommand{Path: "/native/server", Args: []string{"serve"}}
	server, err := StartOwnedServer(context.Background(), OwnedServerConfig{
		Command:         command,
		Endpoint:        unusedLoopbackEndpoint(t),
		Auth:            auth,
		Supervisor:      supervisor,
		StartupTimeout:  time.Second,
		ProbeInterval:   time.Millisecond,
		InterruptGrace:  time.Millisecond,
		TerminateGrace:  time.Millisecond,
		ShutdownTimeout: time.Second,
		Ready: func(context.Context, *Client) error {
			if probes.Add(1) < 2 {
				return errors.New("not ready")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := server.Ref(); got != ref {
		t.Fatalf("owned ref = %+v, want %+v", got, ref)
	}
	if !reflect.DeepEqual(supervisor.started, []productruntime.NativeCommand{command}) {
		t.Fatalf("started commands = %#v", supervisor.started)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := supervisor.signalSnapshot(); !reflect.DeepEqual(got, []signaledRef{
		{ref: ref, signal: productruntime.ProcessInterrupt},
		{ref: ref, signal: productruntime.ProcessTerminate},
	}) {
		t.Fatalf("signals = %#v", got)
	}
	if got := supervisor.waits.Load(); got != 1 {
		t.Fatalf("Wait calls = %d, want 1", got)
	}
}

func TestOwnedServerStartupFailureCleansExactChildAndExistingEndpointStartsNothing(t *testing.T) {
	auth := testAuth(t)
	ref := productruntime.OwnedProcessRef{
		Process:      procinfo.Identity{PID: 5252, Start: "start", StrongStart: "strong"},
		ProcessGroup: 5252,
	}
	supervisor := newFakeOwnedSupervisor(ref)
	supervisor.exitOn = productruntime.ProcessKill
	_, err := StartOwnedServer(context.Background(), OwnedServerConfig{
		Command:         productruntime.NativeCommand{Path: "/native/server"},
		Endpoint:        unusedLoopbackEndpoint(t),
		Auth:            auth,
		Supervisor:      supervisor,
		StartupTimeout:  5 * time.Millisecond,
		ProbeInterval:   time.Millisecond,
		InterruptGrace:  time.Millisecond,
		TerminateGrace:  time.Millisecond,
		ShutdownTimeout: time.Second,
		Ready: func(context.Context, *Client) error {
			return errors.New("not ready")
		},
	})
	if !errors.Is(err, ErrServerNotReady) {
		t.Fatalf("startup error = %v, want ErrServerNotReady", err)
	}
	wantSignals := []signaledRef{
		{ref: ref, signal: productruntime.ProcessInterrupt},
		{ref: ref, signal: productruntime.ProcessTerminate},
		{ref: ref, signal: productruntime.ProcessKill},
	}
	if got := supervisor.signalSnapshot(); !reflect.DeepEqual(got, wantSignals) {
		t.Fatalf("cleanup signals = %#v, want %#v", got, wantSignals)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	occupied := newFakeOwnedSupervisor(ref)
	_, err = StartOwnedServer(context.Background(), OwnedServerConfig{
		Command:         productruntime.NativeCommand{Path: "/native/server"},
		Endpoint:        "http://" + listener.Addr().String(),
		Auth:            auth,
		Supervisor:      occupied,
		StartupTimeout:  time.Second,
		ShutdownTimeout: time.Second,
		Ready:           func(context.Context, *Client) error { return nil },
	})
	if !errors.Is(err, ErrEndpointInUse) {
		t.Fatalf("occupied endpoint error = %v, want ErrEndpointInUse", err)
	}
	if len(occupied.started) != 0 {
		t.Fatalf("started process for occupied endpoint: %#v", occupied.started)
	}
}

type signaledRef struct {
	ref    productruntime.OwnedProcessRef
	signal productruntime.ProcessSignal
}

type fakeOwnedSupervisor struct {
	mu      sync.Mutex
	ref     productruntime.OwnedProcessRef
	started []productruntime.NativeCommand
	signals []signaledRef
	exitOn  productruntime.ProcessSignal
	exit    chan struct{}
	once    sync.Once
	waits   atomic.Int32
}

func newFakeOwnedSupervisor(ref productruntime.OwnedProcessRef) *fakeOwnedSupervisor {
	return &fakeOwnedSupervisor{ref: ref, exit: make(chan struct{})}
}

func (f *fakeOwnedSupervisor) Start(_ context.Context, command productruntime.NativeCommand) (productruntime.OwnedProcessRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, command)
	return f.ref, nil
}

func (f *fakeOwnedSupervisor) Signal(_ context.Context, ref productruntime.OwnedProcessRef, signal productruntime.ProcessSignal) error {
	f.mu.Lock()
	f.signals = append(f.signals, signaledRef{ref: ref, signal: signal})
	f.mu.Unlock()
	if signal == f.exitOn {
		f.once.Do(func() { close(f.exit) })
	}
	return nil
}

func (f *fakeOwnedSupervisor) Wait(ctx context.Context, _ productruntime.OwnedProcessRef) (productruntime.ProcessExit, error) {
	f.waits.Add(1)
	select {
	case <-f.exit:
		return productruntime.ProcessExit{ExitLike: 0}, nil
	case <-ctx.Done():
		return productruntime.ProcessExit{}, ctx.Err()
	}
}

func (f *fakeOwnedSupervisor) signalSnapshot() []signaledRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]signaledRef(nil), f.signals...)
}

func unusedLoopbackEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + address
}
