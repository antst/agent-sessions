package bridge

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAppServer struct {
	listener      net.Listener
	requests      chan map[string]any
	handler       func(map[string]any) (any, error)
	afterResponse func(net.Conn, map[string]any)
	done          chan struct{}
	once          sync.Once
}

func startFakeNativeAppServer(t *testing.T, handler func(map[string]any) (any, error)) (*fakeAppServer, string) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "app-server.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAppServer{
		listener: listener, requests: make(chan map[string]any, 64), handler: handler, done: make(chan struct{}),
	}
	go fake.accept()
	t.Cleanup(fake.close)
	return fake, socket
}

func TestAppServerClientCapturesUnixPeerProcessIdentity(t *testing.T) {
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		if stringValue(request["method"]) == "initialize" {
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if client.peerPID != os.Getpid() || client.peerProcStart == "" ||
		!exactProcessIdentityMatch(client.peerPID, client.peerProcStart) {
		t.Fatalf("App Server peer identity = %d/%q", client.peerPID, client.peerProcStart)
	}
}

func (f *fakeAppServer) close() {
	f.once.Do(func() {
		close(f.done)
		_ = f.listener.Close()
	})
}

func (f *fakeAppServer) accept() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.serve(conn)
	}
}

func (f *fakeAppServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	key := request.Header.Get("Sec-WebSocket-Key")
	digest := sha1.Sum([]byte(key + websocketGUID))
	_, _ = fmt.Fprintf(conn,
		"HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(digest[:]),
	)
	for {
		payload, err := readTestFrame(reader)
		if err != nil {
			return
		}
		var message map[string]any
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		select {
		case f.requests <- message:
		case <-f.done:
			return
		}
		id, hasID := message["id"]
		if !hasID {
			continue
		}
		result, callErr := f.handler(message)
		response := map[string]any{"id": id, "result": result}
		if callErr != nil {
			response = map[string]any{"id": id, "error": map[string]any{"code": -32603, "message": callErr.Error()}}
		}
		body, _ := json.Marshal(response)
		if writeTestFrame(conn, body) != nil {
			return
		}
		if f.afterResponse != nil {
			f.afterResponse(conn, message)
		}
	}
}

func readTestFrame(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		wide := make([]byte, 2)
		if _, err := io.ReadFull(reader, wide); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(wide))
	case 127:
		wide := make([]byte, 8)
		if _, err := io.ReadFull(reader, wide); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(wide)
	}
	var mask []byte
	if header[1]&0x80 != 0 {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(reader, mask); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	for i := range payload {
		if len(mask) > 0 {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, nil
}

func writeTestFrame(writer io.Writer, payload []byte) error {
	header := []byte{0x81, 0}
	switch {
	case len(payload) < 126:
		header[1] = byte(len(payload))
	case len(payload) <= 0xffff:
		header[1] = 126
		wide := make([]byte, 2)
		binary.BigEndian.PutUint16(wide, uint16(len(payload)))
		header = append(header, wide...)
	default:
		header[1] = 127
		wide := make([]byte, 8)
		binary.BigEndian.PutUint64(wide, uint64(len(payload)))
		header = append(header, wide...)
	}
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}

func TestNativeAppServerClientRoundTrip(t *testing.T) {
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{"userAgent": "fake"}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": "thread-a", "status": map[string]any{"type": "idle"}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	var result struct {
		Thread appThread `json:"thread"`
	}
	if err := client.request(ctx, "thread/read", map[string]any{"threadId": "thread-a"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Thread.ID != "thread-a" || statusType(result.Thread.Status) != "idle" {
		t.Fatalf("unexpected thread result: %+v", result.Thread)
	}
}

func TestActiveAppServerThreads(t *testing.T) {
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{"idle-thread", "active-b", "active-a"}}, nil
		case "thread/read":
			params := request["params"].(map[string]any)
			threadID := stringValue(params["threadId"])
			status := "idle"
			if strings.HasPrefix(threadID, "active-") {
				status = "active"
			}
			return map[string]any{"thread": map[string]any{
				"id": threadID, "status": map[string]any{"type": status},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	active, err := activeAppServerThreads(client)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(active, []string{"active-a", "active-b"}) {
		t.Fatalf("active threads = %v", active)
	}
}

func TestAppServerSocketRunning(t *testing.T) {
	_, socket := startFakeNativeAppServer(t, func(map[string]any) (any, error) {
		return map[string]any{}, nil
	})
	running, err := appServerSocketRunning(socket)
	if err != nil || !running {
		t.Fatalf("live socket: running=%v err=%v", running, err)
	}

	missing := filepath.Join(t.TempDir(), "missing.sock")
	running, err = appServerSocketRunning(missing)
	if err != nil || running {
		t.Fatalf("missing socket: running=%v err=%v", running, err)
	}
}

func TestNativeAppServerClientDispatchesDynamicMCPTool(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	threadID := "00000000-0000-0000-0000-000000000031"
	writeTestActiveCodexLane(t, resolveNativePaths(), threadID)
	fake, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize", "trigger/dynamic":
			return map[string]any{}, nil
		case "mcpServer/tool/call":
			t.Fatal("agent_sessions dynamic tools must be handled with the App Server-attested thread id")
			return nil, errors.New("unexpected agent_sessions MCP relay")
		default:
			return map[string]any{}, nil
		}
	})
	fake.afterResponse = func(conn net.Conn, request map[string]any) {
		if stringValue(request["method"]) != "trigger/dynamic" {
			return
		}
		body, _ := json.Marshal(map[string]any{
			"id": "dynamic-request-1", "method": "item/tool/call",
			"params": map[string]any{
				"threadId": threadID, "turnId": "turn-dynamic", "callId": "call-dynamic",
				"namespace": nil, "tool": "mcp__agent_sessions__list_peers", "arguments": map[string]any{},
			},
		})
		_ = writeTestFrame(conn, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if err := client.request(ctx, "trigger/dynamic", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case request := <-fake.requests:
			if request["id"] != "dynamic-request-1" || request["method"] != nil {
				continue
			}
			result := request["result"].(map[string]any)
			if success, _ := result["success"].(bool); success {
				t.Fatalf("ungrouped dynamic call unexpectedly succeeded: %v", result)
			}
			items := result["contentItems"].([]any)
			if len(items) != 1 || !strings.Contains(items[0].(map[string]any)["text"].(string), "communication is inactive for this ungrouped session") {
				t.Fatalf("unexpected dynamic content: %v", items)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for dynamic tool response")
		}
	}
}

func TestDynamicPeerToolCannotClaimAnotherThread(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PEER_DATA_DIR", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	callerID := "00000000-0000-0000-0000-000000000041"
	writeTestActiveCodexLane(t, resolveNativePaths(), callerID)
	fake, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize", "trigger/dynamic":
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fake.afterResponse = func(conn net.Conn, request map[string]any) {
		if stringValue(request["method"]) != "trigger/dynamic" {
			return
		}
		body, _ := json.Marshal(map[string]any{
			"id": "dynamic-foreign-session", "method": "item/tool/call",
			"params": map[string]any{
				"threadId": "00000000-0000-0000-0000-000000000041", "turnId": "turn-dynamic", "callId": "call-dynamic",
				"namespace": nil, "tool": "mcp__agent_sessions__identity",
				"arguments": map[string]any{"session_id": "00000000-0000-0000-0000-000000000042"},
			},
		})
		_ = writeTestFrame(conn, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if err := client.request(ctx, "trigger/dynamic", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case request := <-fake.requests:
			if request["id"] != "dynamic-foreign-session" || request["method"] != nil {
				continue
			}
			result := request["result"].(map[string]any)
			if success, _ := result["success"].(bool); success {
				t.Fatalf("foreign session claim succeeded: %v", result)
			}
			items := result["contentItems"].([]any)
			if len(items) != 1 || !strings.Contains(items[0].(map[string]any)["text"].(string), "cannot act as") {
				t.Fatalf("unexpected identity rejection: %v", items)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for dynamic identity rejection")
		}
	}
}

func TestNativeSupervisorWakesIdleThreadInYoloMode(t *testing.T) {
	requests := make(chan map[string]any, 64)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		requests <- request
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{"userAgent": "fake"}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{}}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{
				"id": "thread-idle", "cwd": t.TempDir(), "source": "appServer", "status": map[string]any{"type": "idle"},
			}}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-wake", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	executable, _ := os.Executable()
	supervisor := &nativeSupervisor{
		paths: nativePaths{
			dataRoot: filepath.Join(root, "state"), claudeRoot: filepath.Join(root, "claude"),
			codexHome: filepath.Join(root, "codex"), runtimeDir: filepath.Join(root, "run"),
			supervisorSock: filepath.Join(root, "supervisor.sock"), supervisorState: filepath.Join(root, "supervisor.json"),
			appServerSock: socket,
		},
		pluginVersion: "test", executable: executable, procStart: readProcStart(os.Getpid()),
		startedAt: time.Now().UnixMilli(), done: make(chan struct{}), shims: map[string]map[string]any{},
		activeTurns: map[string]string{}, subscribed: map[string]bool{},
	}
	if err := supervisor.start(); err != nil {
		t.Fatal(err)
	}
	defer supervisor.shutdown()
	writeTestActiveCodexLane(t, supervisor.paths, "thread-idle")
	delivery, err := supervisor.wakeThread("thread-idle", map[string]any{
		"message": "WAKE", "from": "uds:/tmp/claude.sock", "fromName": "peer-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if delivery != "started" {
		t.Fatalf("unexpected delivery: %s", delivery)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case request := <-requests:
			if stringValue(request["method"]) != "turn/start" {
				continue
			}
			params := request["params"].(map[string]any)
			if _, present := params["approvalPolicy"]; present {
				t.Fatalf("peer wake persisted approval override: %v", params["approvalPolicy"])
			}
			if _, present := params["sandboxPolicy"]; present {
				t.Fatalf("peer wake persisted sandbox override: %v", params["sandboxPolicy"])
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for turn/start")
		}
	}
}

func TestNativeSupervisorDoesNotRepublishRetiredLoadedThread(t *testing.T) {
	threadReads := make(chan struct{}, 1)
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{"archived-lane"}}, nil
		case "thread/list":
			params, _ := request["params"].(map[string]any)
			if archived, _ := params["archived"].(bool); archived {
				return map[string]any{"data": []map[string]any{{"id": "archived-lane"}}}, nil
			}
			return map[string]any{"data": []any{}}, nil
		case "thread/read":
			select {
			case threadReads <- struct{}{}:
			default:
			}
			return nil, errors.New("retired thread must not be read or republished")
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	supervisor := &nativeSupervisor{
		paths: nativePaths{
			dataRoot: filepath.Join(root, "state"), claudeRoot: filepath.Join(root, "claude"),
			codexHome: filepath.Join(root, "codex"), runtimeDir: filepath.Join(root, "run"),
			supervisorSock:  filepath.Join(root, "runtime", "supervisor.sock"),
			supervisorState: filepath.Join(root, "state", "supervisor.json"), appServerSock: socket,
		},
		pluginVersion: "test", executable: os.Args[0], shimExecutable: os.Args[0],
		procStart: readProcStart(os.Getpid()), startedAt: time.Now().UnixMilli(), done: make(chan struct{}),
		shims: map[string]map[string]any{}, activeTurns: map[string]string{}, subscribed: map[string]bool{},
		retired: map[string]bool{},
	}
	writeTestActiveCodexLane(t, supervisor.paths, "archived-lane")
	if err := supervisor.start(); err != nil {
		t.Fatal(err)
	}
	defer supervisor.shutdown()
	select {
	case <-threadReads:
		t.Fatal("reconciliation read a retired loaded thread")
	default:
	}
	if len(supervisor.shims) != 0 {
		t.Fatalf("retired thread was republished: %#v", supervisor.shims)
	}
	if !supervisor.isRetired("archived-lane") {
		t.Fatal("startup audit did not retire the archived loaded thread")
	}
	if _, err := os.Stat(retiredThreadPath(supervisor.paths, "archived-lane")); err != nil {
		t.Fatalf("startup audit did not persist retirement: %v", err)
	}
}

func TestNativeSupervisorStartupAuditKeepsActiveLoadedThread(t *testing.T) {
	_, socket := startFakeNativeAppServer(t, func(request map[string]any) (any, error) {
		switch stringValue(request["method"]) {
		case "initialize":
			return map[string]any{}, nil
		case "thread/list":
			return map[string]any{"data": []map[string]any{{"id": "active-lane"}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	root := t.TempDir()
	paths := nativePaths{dataRoot: filepath.Join(root, "state")}
	if err := markRetiredThread(paths, "active-lane"); err != nil {
		t.Fatal(err)
	}
	supervisor := &nativeSupervisor{paths: paths, retired: readRetiredThreads(paths)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if err := supervisor.auditLoadedRetirement(client, []string{"active-lane"}); err != nil {
		t.Fatal(err)
	}
	if supervisor.isRetired("active-lane") {
		t.Fatal("startup audit retained a tombstone for an active thread")
	}
	if _, err := os.Stat(retiredThreadPath(paths, "active-lane")); !os.IsNotExist(err) {
		t.Fatalf("active thread retirement marker survived audit: %v", err)
	}
}
