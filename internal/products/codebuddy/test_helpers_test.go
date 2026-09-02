package codebuddy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type fakeRegistry struct{}
type fakeProcesses struct{}

type memoryReceiptReader struct{ values map[string][]byte }

func (reader memoryReceiptReader) OpenReceipt(id string) (io.ReadCloser, int64, [32]byte, error) {
	body, ok := reader.values[id]
	if !ok {
		return nil, 0, [32]byte{}, errors.New("missing receipt")
	}
	copyBody := append([]byte(nil), body...)
	return io.NopCloser(strings.NewReader(string(copyBody))), int64(len(copyBody)), sha256.Sum256(copyBody), nil
}

type fakeOwnedSupervisor struct {
	mu       sync.Mutex
	commands []productruntime.NativeCommand
	servers  map[int]*http.Server
	exits    map[int]chan struct{}
	handler  func(*fakeNativeServer, http.ResponseWriter, *http.Request)
	native   *fakeNativeServer
	nextPID  int
}

type fakeNativeServer struct {
	mu          sync.Mutex
	job         AgentJob
	getCount    int
	replySaved  bool
	archived    bool
	deleteCount int
	resumeID    string
	sessionID   string
	password    string
	requests    []string
}

func newFakeOwnedSupervisor() *fakeOwnedSupervisor {
	return &fakeOwnedSupervisor{servers: map[int]*http.Server{}, exits: map[int]chan struct{}{}, nextPID: 4000}
}

func (supervisor *fakeOwnedSupervisor) Start(_ context.Context, command productruntime.NativeCommand) (productruntime.OwnedProcessRef, error) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	port := argumentValue(command.Args, "--port")
	password := ""
	for _, variable := range command.SensitiveEnv {
		if variable.Name == GatewayPasswordEnv {
			password = variable.Value.Reveal()
		}
	}
	if port == "" || password == "" {
		return productruntime.OwnedProcessRef{}, errors.New("invalid server command")
	}
	supervisor.nextPID++
	pid := supervisor.nextPID
	identity := procinfo.Identity{PID: pid, Start: fmt.Sprintf("start-%d", pid), StrongStart: fmt.Sprintf("strong-%d", pid)}
	native := &fakeNativeServer{password: password, sessionID: argumentValue(command.Args, "--session-id")}
	supervisor.native = native
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+password || request.Header.Get(CSRFHeader) != CSRFValue {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		native.mu.Lock()
		native.requests = append(native.requests, request.Method+" "+request.URL.Path)
		native.mu.Unlock()
		if supervisor.handler != nil {
			supervisor.handler(native, response, request)
			return
		}
		defaultNativeHandler(native, response, request)
	})
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: handler}
	listener, err := net.Listen("tcp4", server.Addr)
	if err != nil {
		return productruntime.OwnedProcessRef{}, err
	}
	exit := make(chan struct{})
	supervisor.servers[pid] = server
	supervisor.exits[pid] = exit
	supervisor.commands = append(supervisor.commands, command)
	go func() {
		_ = server.Serve(listener)
		close(exit)
	}()
	return productruntime.OwnedProcessRef{Process: identity, ProcessGroup: pid}, nil
}

func (supervisor *fakeOwnedSupervisor) Signal(ctx context.Context, ref productruntime.OwnedProcessRef, _ productruntime.ProcessSignal) error {
	supervisor.mu.Lock()
	server := supervisor.servers[ref.Process.PID]
	supervisor.mu.Unlock()
	if server == nil {
		return nil
	}
	// A process signal terminates the fake native process immediately. Closing
	// active HTTP connections models process exit more faithfully than graceful
	// http.Server shutdown, which can wait on the test client's idle pool.
	return server.Close()
}

func (supervisor *fakeOwnedSupervisor) Wait(ctx context.Context, ref productruntime.OwnedProcessRef) (productruntime.ProcessExit, error) {
	supervisor.mu.Lock()
	exit := supervisor.exits[ref.Process.PID]
	supervisor.mu.Unlock()
	if exit == nil {
		return productruntime.ProcessExit{}, errors.New("unknown process")
	}
	select {
	case <-exit:
		return productruntime.ProcessExit{}, nil
	case <-ctx.Done():
		return productruntime.ProcessExit{}, ctx.Err()
	}
}

func defaultNativeHandler(native *fakeNativeServer, response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	switch request.Method + " " + request.URL.Path {
	case "GET /api/v1/health":
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"status": "ok", "pid": 4001}})
	case "GET /api/v1/sessions/live":
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"sessionId": nil, "writerOccupied": false}})
	case "GET /api/v1/jobs":
		native.mu.Lock()
		jobs := []AgentJob{}
		if native.job.ID != "" {
			jobs = append(jobs, native.job)
		}
		native.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"jobs": jobs}})
	case "POST /api/v1/jobs":
		var body DispatchJobRequest
		_ = json.NewDecoder(request.Body).Decode(&body)
		native.mu.Lock()
		native.job = AgentJob{ID: "job-1", SessionID: "native-job-session-1", State: "working", Name: body.Name, Cwd: body.Cwd, StartedAt: 1, UpdatedAt: 2}
		job := native.job
		native.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"data": job})
	case "GET /api/v1/jobs/job-1":
		native.mu.Lock()
		native.getCount++
		job := native.job
		if native.getCount >= 2 {
			terminalAt := int64(3)
			if job.UpdatedAt >= terminalAt {
				terminalAt = job.UpdatedAt + 1
			}
			job.State, job.Status, job.Detail, job.UpdatedAt, job.FirstTerminalAt = "done", "idle", "native result", terminalAt, terminalAt
			native.job = job
		}
		native.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"job": job}})
	case "GET /api/v1/jobs/job-1/stream":
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "id: 1\nevent: message\ndata: {\"text\":\"stream result\"}\n\n")
	case "POST /api/v1/jobs/job-1/reply":
		native.mu.Lock()
		saved := native.replySaved
		native.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"delivered": !saved, "saved": saved, "notice": "accepted"}})
	case "POST /api/v1/jobs/job-1/respawn":
		native.mu.Lock()
		native.job.State, native.job.UpdatedAt = "working", 4
		job := native.job
		native.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"job": job}})
	case "POST /api/v1/jobs/job-1/stop":
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"stopped": true}})
	case "DELETE /api/v1/jobs/job-1":
		native.mu.Lock()
		native.archived = true
		native.deleteCount++
		native.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true}})
	case "POST /api/v1/jobs/resume":
		var body struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		job := AgentJob{ID: "job-1", SessionID: body.SessionID, State: "done", Cwd: request.URL.Query().Get("cwd"), StartedAt: 1, UpdatedAt: 1}
		native.mu.Lock()
		native.job, native.resumeID = job, body.SessionID
		native.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"data": job})
	default:
		http.NotFound(response, request)
	}
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func freeEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	_ = listener.Close()
	return endpoint
}

func argumentValue(arguments []string, name string) string {
	for index := range arguments {
		if arguments[index] == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}

func codebuddyTestConfig(t *testing.T, _ any, _ any) Config {
	t.Helper()
	return Config{
		Executable: "/opt/codebuddy", MCPConfigPath: "/opt/agent-sessions/codebuddy-mcp.json",
		Endpoints: EndpointAllocatorFunc(func(context.Context) (string, error) { return freeEndpoint(t), nil }),
		Secrets: SecretSourceFunc(func(context.Context) (productruntime.SensitiveValue, error) {
			return productruntime.NewSensitiveValue("test-lane-secret-not-for-production"), nil
		}),
		Recovery: RecoveryRequestSourceFunc(func(_ context.Context, request productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error) {
			return productruntime.LaneOpenRequest{
				ProductID: ProductID, LaneID: request.LaneID, ResumeNativeID: request.PriorNativeSessionID,
				Cwd: "/work", PermissionMode: permissionmode.Default,
			}, nil
		}),
		PollInterval: time.Millisecond,
	}
}
