package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestLiveRPCErrorTableClassifiesWithoutMaskingProductFailures(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   int
		reason string
		data   string
	}{
		{name: "unknown", err: &federationpkg.UnknownTargetError{Target: "missing", Detail: "missing"}, code: livepresence.Unknown, data: `{"target":"missing"}`},
		{name: "invalid", err: livepresence.ClassifyError(livepresence.ErrInvalidParams, errors.New("bad")), code: livepresence.InvalidParams, data: `{"method":"lane.steer"}`},
		{name: "not permitted", err: livepresence.ClassifyError(livepresence.ErrNotPermitted, errors.New("denied")), code: livepresence.NotPermitted, data: `{"method":"lane.steer"}`},
		{name: "busy", err: livepresence.BusyError("busy-native", errors.New("exact busy detail")), code: livepresence.Busy, data: `{"uuid":"busy-native"}`},
		{name: "no running turn", err: livepresence.ClassifyError(livepresence.ErrNoRunningTurn, errors.New("exact turn detail")), code: livepresence.NotPermitted, reason: "no running turn"},
		{name: "steer unsupported", err: productruntime.ErrUnsupportedSteer, code: livepresence.NotPermitted, reason: "steer unsupported"},
		{name: "unavailable", err: livepresence.ProductError("qwen", productruntime.ErrUnavailable), code: livepresence.ProductUnavailable, data: `{"product":"qwen"}`},
		{name: "ambiguous is not busy", err: productruntime.ErrAmbiguousSession, code: livepresence.ProductFailure},
		{name: "product failure", err: errors.New("product exact failure"), code: livepresence.ProductFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := livepresence.FailureFromError(json.RawMessage(`1`), "lane.steer", test.err)
			if frame.Error == nil || frame.Error.Code != test.code {
				t.Fatalf("error frame = %+v", frame)
			}
			if test.code == livepresence.ProductFailure && frame.Error.Message != test.err.Error() {
				t.Fatalf("product message = %q", frame.Error.Message)
			}
			if test.code == livepresence.ProductFailure && !strings.Contains(string(frame.Error.Data), `"agent_sessions_bug_report"`) {
				t.Fatalf("unexpected failure has no bug-report guidance: %s", frame.Error.Data)
			}
			if test.reason != "" && !strings.Contains(string(frame.Error.Data), `"reason":"`+test.reason+`"`) {
				t.Fatalf("error data = %s", frame.Error.Data)
			}
			if test.data != "" && string(frame.Error.Data) != test.data {
				t.Fatalf("error data = %s, want %s", frame.Error.Data, test.data)
			}
		})
	}
	structured := livepresence.NewError(livepresence.ProductFailure, "native exact failure", map[string]any{"detail": "native exact failure"})
	frame := livepresence.FailureFromError(json.RawMessage(`2`), "lane.turn.wait", structured)
	if frame.Error == nil || string(frame.Error.Data) != `{"detail":"native exact failure"}` {
		t.Fatalf("structured native failure was changed: %+v", frame.Error)
	}
	for _, malformed := range []*livepresence.RPCError{
		{Code: livepresence.Busy, Message: "Session busy", Data: json.RawMessage(`{"uuid":"bad/id"}`)},
		{Code: livepresence.Busy, Message: "busy", Data: json.RawMessage(`{"uuid":"busy-native"}`)},
		{Code: livepresence.NotPermitted, Message: "Operation not permitted", Data: json.RawMessage(`{"method":"lane.steer","group":"team"}`)},
		{Code: livepresence.ProductUnavailable, Message: "Product not launchable", Data: json.RawMessage(`{"product":"Bad"}`)},
	} {
		if got := livepresence.FailureFromError(json.RawMessage(`3`), "lane.steer", malformed); got.Error == nil || got.Error.Code != livepresence.ProductFailure || !strings.Contains(string(got.Error.Data), `"agent_sessions_bug_report"`) {
			t.Fatalf("malformed structured error was relayed: %+v", got.Error)
		}
	}
	frame = livepresence.FailureFromError(json.RawMessage(`4`), "message.send", &federationpkg.GroupNotPermittedError{Group: "other"})
	if frame.Error == nil || frame.Error.Code != livepresence.NotPermitted || string(frame.Error.Data) != `{"group":"other"}` {
		t.Fatalf("forbidden group data = %+v", frame.Error)
	}
}

func TestLiveSessionReconnectCadenceIsTwoSeconds(t *testing.T) {
	if livepresence.ReconnectInterval != 2*time.Second {
		t.Fatalf("live reconnect interval = %s", livepresence.ReconnectInterval)
	}
}

func testPresenceServer(t *testing.T) *livePresenceServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(livepresence.Report) {}, func(livepresence.Report) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func openTestPresence(t *testing.T, server *livePresenceServer, identity string) (net.Conn, *json.Encoder, *json.Decoder) {
	return openTestPresenceAs(t, server, identity, identity)
}

func openTestPresenceAs(t *testing.T, server *livePresenceServer, identity, name string) (net.Conn, *json.Encoder, *json.Decoder) {
	t.Helper()
	connection, err := net.Dial("unix", server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	encoder, decoder := json.NewEncoder(connection), json.NewDecoder(connection)
	if identity != "" {
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "hello", "method": "session.hello", "params": map[string]any{
			"protocol": 1, "uuid": identity, "name": name, "groups": []string{}, "product": "future", "info": map[string]string{},
		}}); err != nil {
			t.Fatal(err)
		}
		var response livepresence.Frame
		if err := decoder.Decode(&response); err != nil || response.Error != nil {
			t.Fatalf("hello = %+v, %v", response, err)
		}
	}
	return connection, encoder, decoder
}

func TestLivePresenceRejectsEveryPreHelloOrNonRequestFrame(t *testing.T) {
	server := testPresenceServer(t)
	for _, line := range []string{
		`{"uuid":"legacy","name":"legacy","groups":[],"product":"codex"}`,
		`[{"jsonrpc":"2.0","id":1,"method":"session.hello","params":{}}]`,
		`{"jsonrpc":"2.0","method":"session.hello","params":{}}`,
		`not-json`,
	} {
		connection, dialErr := net.Dial("unix", server.listener.Addr().String())
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		if _, writeErr := fmt.Fprintln(connection, line); writeErr != nil {
			t.Fatal(writeErr)
		}
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		if _, readErr := bufio.NewReader(connection).ReadByte(); !errors.Is(readErr, io.EOF) {
			t.Fatalf("frame %q was not closed: %v", line, readErr)
		}
		_ = connection.Close()
	}
}

func TestLivePresenceJSONRPCIDRange(t *testing.T) {
	valid := map[string]bool{`"opaque"`: true, `9007199254740991`: true, `-9007199254740991`: true, `1e3`: true, `1.0`: true}
	for _, id := range []string{`9007199254740992`, `-9007199254740992`, `1.5`, `1e-3`, `null`} {
		valid[id] = false
	}
	for id, want := range valid {
		frame := livepresence.Frame{JSONRPC: "2.0", ID: json.RawMessage(id), Method: "probe", Params: json.RawMessage(`{}`)}
		if got := livepresence.ValidRequest(frame); got != want {
			t.Fatalf("id %s valid = %t, want %t", id, got, want)
		}
	}
}

func TestLivePresenceClosesSecondHelloAndPostHandshakeNotification(t *testing.T) {
	server := testPresenceServer(t)
	for name, next := range map[string]map[string]any{
		"second hello": {"jsonrpc": "2.0", "id": "again", "method": "session.hello", "params": map[string]any{}},
		"notification": {"jsonrpc": "2.0", "method": "session.update", "params": map[string]any{"name": "after", "info": map[string]string{}}},
	} {
		t.Run(name, func(t *testing.T) {
			identityName := strings.ReplaceAll(name, " ", "-")
			_, encoder, decoder := openTestPresence(t, server, "native-"+identityName)
			var response livepresence.Frame
			if err := encoder.Encode(next); err != nil {
				t.Fatal(err)
			}
			if err := decoder.Decode(&response); !errors.Is(err, io.EOF) {
				t.Fatalf("post-handshake %s was answered: %+v, %v", name, response, err)
			}
		})
	}
}

func TestLivePresenceRejectsUnsupportedVersionAndGroupUpdate(t *testing.T) {
	server := testPresenceServer(t)
	connection, encoder, decoder := openTestPresence(t, server, "")
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session.hello", "params": map[string]any{
		"protocol": 2, "uuid": "version", "name": "version", "groups": []string{}, "product": "future", "info": map[string]string{},
	}}); err != nil {
		t.Fatal(err)
	}
	var response livepresence.Frame
	if err := decoder.Decode(&response); err != nil || response.Error == nil || response.Error.Code != livepresence.UnsupportedVersion {
		t.Fatalf("unsupported-version response = %+v, %v", response, err)
	}
	if err := decoder.Decode(&response); !errors.Is(err, io.EOF) {
		t.Fatalf("unsupported-version connection remained open: %v", err)
	}
	_ = connection.Close()
	_, encoder, decoder = openTestPresence(t, server, "")
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "hello", "method": "session.hello", "params": map[string]any{
		"protocol": 1, "uuid": "native id", "name": "worker", "groups": []string{}, "product": "future", "info": map[string]string{},
	}}); err != nil {
		t.Fatal(err)
	}
	response = livepresence.Frame{}
	if err := decoder.Decode(&response); err != nil || response.Error == nil || response.Error.Code != livepresence.InvalidParams || string(response.Error.Data) != `{"method":"session.hello"}` {
		t.Fatalf("invalid hello response = %+v, %v", response, err)
	}
	if err := decoder.Decode(&response); !errors.Is(err, io.EOF) {
		t.Fatalf("invalid hello connection remained open: %v", err)
	}
	_, encoder, decoder = openTestPresence(t, server, "update")
	response = livepresence.Frame{}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "update", "method": "session.update", "params": map[string]any{
		"name": "after", "info": map[string]string{}, "groups": []string{"other"},
	}}); err != nil {
		t.Fatal(err)
	}
	response = livepresence.Frame{}
	if err := decoder.Decode(&response); err != nil || response.Error == nil || response.Error.Code != livepresence.InvalidParams {
		t.Fatalf("group-update response = %+v, %v", response, err)
	}
}

func TestLiveHelloCapabilitiesAreClosedAndOnlyAdvertiseTrueLaneSupport(t *testing.T) {
	base := `{"protocol":1,"uuid":"session","name":"","groups":[],"product":"dsh","info":{},"capabilities":%s}`
	for _, invalid := range []string{`{}`, `{"lane":false}`, `{"future":true}`} {
		if _, _, err := livepresence.DecodeHello(json.RawMessage(fmt.Sprintf(base, invalid))); err == nil {
			t.Fatalf("invalid capabilities accepted: %s", invalid)
		}
	}
	report, protocol, err := livepresence.DecodeHello(json.RawMessage(fmt.Sprintf(base, `{"lane":true}`)))
	if err != nil || protocol != 1 || !report.Capabilities.Lane {
		t.Fatalf("lane hello = %+v, protocol=%d, err=%v", report, protocol, err)
	}
}

func TestLiveHelloRejectsEveryInvalidIdentityShape(t *testing.T) {
	base := livepresence.Report{UUID: "ses_native", Name: "Native Worker/One", Groups: []string{"team!", "session:host/ses_native"}, Product: "future", Info: map[string]string{}}
	reject := func(value livepresence.Report) {
		raw, _ := json.Marshal(struct {
			Protocol int `json:"protocol"`
			livepresence.Report
		}{1, value})
		if _, _, err := livepresence.DecodeHello(raw); err == nil {
			t.Fatalf("invalid hello accepted: %s", raw)
		}
	}
	for _, invalid := range []string{"native id", "native\n", "native/id", strings.Repeat("u", 129)} {
		value := livepresence.CloneReport(base)
		value.UUID = invalid
		reject(value)
	}
	for _, invalid := range []string{"native\tworker", strings.Repeat("n", 257)} {
		value := livepresence.CloneReport(base)
		value.Name = invalid
		reject(value)
	}
	for _, invalid := range []string{"", "two words", "team\n", "team/one", "session:host", strings.Repeat("g", 193)} {
		value := livepresence.CloneReport(base)
		value.Groups = []string{invalid}
		reject(value)
	}
	value := livepresence.CloneReport(base)
	value.Product = "Future"
	reject(value)
}

func TestLiveUpdateKeepsTheNameIdentityShape(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"name":"bad\nname","info":{}}`),
		json.RawMessage(`{"name":"` + strings.Repeat("n", 257) + `","info":{}}`),
	} {
		if _, _, err := livepresence.DecodeUpdate(raw); err == nil {
			t.Fatalf("invalid update accepted: %s", raw)
		}
	}
}

func TestLivePresenceNewerSameUUIDConnectionReplacesOlderWithoutRemovingIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan livepresence.Report, 2)
	left := make(chan livepresence.Report, 2)
	var server *livePresenceServer
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(report livepresence.Report) { joined <- report }, func(livepresence.Report) {}, nil, func(report livepresence.Report) {
		if !server.mu.TryLock() {
			panic("post-leave callback ran under presence lock")
		}
		server.mu.Unlock()
		left <- report
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	first, _, _ := openTestPresenceAs(t, server, "same", "first")
	if got := <-joined; got.Name != "first" {
		t.Fatalf("first join = %+v", got)
	}
	second, _, _ := openTestPresenceAs(t, server, "same", "second")
	if got := <-joined; got.Name != "second" {
		t.Fatalf("replacement join = %+v", got)
	}
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, readErr := bufio.NewReader(first).ReadByte(); !errors.Is(readErr, io.EOF) {
		t.Fatalf("older connection was not closed: %v", readErr)
	}
	select {
	case report := <-left:
		t.Fatalf("older connection removed replacement: %+v", report)
	case <-time.After(50 * time.Millisecond):
	}
	_ = second.Close()
	select {
	case report := <-left:
		if report.Name != "second" {
			t.Fatalf("leave report = %+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement close was not observed")
	}
}

func TestDisplacedLivePresenceUpdateIsRejectedWithoutMutatingReplacement(t *testing.T) {
	oldLocal, oldRemote := net.Pipe()
	defer oldLocal.Close()
	defer oldRemote.Close()
	oldConnection := livepresence.NewConnection(oldLocal)
	oldConnection.SetReport(livepresence.Report{UUID: "same", Name: "old", Product: "future", Groups: []string{}, Info: map[string]string{}})
	newConnection := livepresence.NewConnection(oldLocal)
	newConnection.SetReport(livepresence.Report{UUID: "same", Name: "replacement", Product: "future", Groups: []string{}, Info: map[string]string{}})
	server := &livePresenceServer{current: map[string]*livepresence.Connection{"same": newConnection}}
	params := json.RawMessage(`{"name":"displaced","info":{"cwd":"/wrong"}}`)
	done := make(chan struct{})
	go func() {
		server.handleRequest(context.Background(), oldConnection, livepresence.Frame{
			JSONRPC: "2.0", ID: json.RawMessage(`"update"`), Method: "session.update", Params: params,
		})
		close(done)
	}()
	var response livepresence.Frame
	if err := json.NewDecoder(oldRemote).Decode(&response); err != nil || response.Error == nil || response.Error.Code != livepresence.NotPermitted ||
		string(response.Error.Data) != `{"method":"session.update"}` {
		t.Fatalf("displaced update response = %+v, %v", response, err)
	}
	<-done
	if got := oldConnection.Report(); got.Name != "old" || got.Info["cwd"] != "" {
		t.Fatalf("displaced report mutated = %+v", got)
	}
	if got := newConnection.Report(); got.Name != "replacement" || got.Info["cwd"] != "" {
		t.Fatalf("replacement report mutated = %+v", got)
	}
}

func TestLivePresenceUpdateAndReplacementAreLinearized(t *testing.T) {
	oldLocal, oldRemote := net.Pipe()
	defer oldLocal.Close()
	defer oldRemote.Close()
	oldConnection := livepresence.NewConnection(oldLocal)
	oldConnection.SetReport(livepresence.Report{UUID: "same", Name: "old"})
	replacement := livepresence.NewConnection(oldLocal)
	entered, release := make(chan struct{}), make(chan struct{})
	server := &livePresenceServer{
		current: map[string]*livepresence.Connection{"same": oldConnection},
		join: func(report livepresence.Report) {
			if report.Name == "updated" {
				close(entered)
				<-release
			}
		},
	}
	updated := make(chan struct{})
	go func() {
		server.handleRequest(context.Background(), oldConnection, livepresence.Frame{
			JSONRPC: "2.0", ID: json.RawMessage(`"update"`), Method: "session.update", Params: json.RawMessage(`{"name":"updated","info":{"cwd":"/old"}}`),
		})
		close(updated)
	}()
	<-entered
	replaced := make(chan struct{})
	go func() {
		server.mu.Lock()
		server.current["same"] = replacement
		server.mu.Unlock()
		close(replaced)
	}()
	select {
	case <-replaced:
		t.Fatal("replacement overtook the checked current update")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	var response livepresence.Frame
	if err := json.NewDecoder(oldRemote).Decode(&response); err != nil || response.Error != nil {
		t.Fatalf("update response = %+v, %v", response, err)
	}
	<-updated
	<-replaced
	if got := oldConnection.Report(); got.Name != "updated" || got.Info["cwd"] != "/old" {
		t.Fatalf("linearized update = %+v", got)
	}
	if got := server.current["same"]; got != replacement {
		t.Fatalf("current connection = %p, want %p", got, replacement)
	}
}

func TestLivePresenceConnectionDefinesSessionLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan livepresence.Report, 1)
	left := make(chan livepresence.Report, 1)
	root := t.TempDir()
	server, err := startLivePresenceServer(ctx, root, func(report livepresence.Report) {
		joined <- report
	}, func(report livepresence.Report) {
		left <- report
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := livepresence.Report{UUID: "session-1", Name: "builder", Groups: []string{"team", "team"}, Product: "codex"}
	clientCtx, stopClient := context.WithCancel(context.Background())
	livepresence.StartClient(clientCtx, server.listener.Addr().String(), report, nil)
	select {
	case got := <-joined:
		if got.UUID != report.UUID || got.Name != report.Name || !reflect.DeepEqual(got.Groups, []string{"team", "team"}) {
			t.Fatalf("join report = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("presence report was not joined")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-left:
		if got.UUID != report.UUID {
			t.Fatalf("leave report = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("presence disconnect was not observed")
	}
	rejoined := make(chan livepresence.Report, 1)
	successor, err := startLivePresenceServer(ctx, root, func(report livepresence.Report) {
		rejoined <- report
	}, func(livepresence.Report) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Close() })
	select {
	case got := <-rejoined:
		if got.UUID != report.UUID {
			t.Fatalf("reconnect report = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live session did not reconnect to the successor daemon")
	}
	stopClient()
}

func TestResolvingLivePresenceDoesNotHelloUntilProductConfirmsIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan livepresence.Report, 1)
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(report livepresence.Report) {
		joined <- report
	}, func(livepresence.Report) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	attempted := make(chan struct{}, 1)
	confirmed := make(chan struct{})
	client := livepresence.StartResolvingClient(ctx, server.listener.Addr().String(), func(context.Context) (livepresence.Report, bool) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		select {
		case <-confirmed:
			return livepresence.Report{
				UUID: "product-session", Name: "product-name", Groups: []string{"team"}, Product: "claude", Info: map[string]string{},
			}, true
		default:
			return livepresence.Report{}, false
		}
	}, nil)
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("product identity resolver was not called")
	}
	if _, err := client.Call(ctx, "probe", "peers.list", map[string]any{}); err == nil || err.Error() != "live session identity is not confirmed by the product" {
		t.Fatalf("unconfirmed connector call error = %v", err)
	}
	select {
	case report := <-joined:
		t.Fatalf("unconfirmed product identity sent hello: %+v", report)
	case <-time.After(50 * time.Millisecond):
	}
	close(confirmed)
	select {
	case report := <-joined:
		if report.UUID != "product-session" || report.Name != "product-name" {
			t.Fatalf("confirmed product report = %+v", report)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("confirmed product identity did not reconnect")
	}
}

func TestLivePresenceClientProjectsNameOnTheSameConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan livepresence.Report, 2)
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(report livepresence.Report) {
		joined <- report
	}, func(livepresence.Report) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	report := livepresence.Report{UUID: "session", Name: "session", Groups: []string{"group"}, Product: "qwen"}
	client := livepresence.StartClient(ctx, server.listener.Addr().String(), report, nil)
	select {
	case initial := <-joined:
		if initial.Name != "session" {
			t.Fatalf("initial name = %q", initial.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initial live report was not received")
	}
	report.Name = "product-title"
	updateCtx, stopUpdate := context.WithTimeout(ctx, 2*time.Second)
	defer stopUpdate()
	if err := client.UpdateReport(updateCtx, report); err != nil {
		t.Fatal(err)
	}
	select {
	case updated := <-joined:
		if updated.Name != "product-title" {
			t.Fatalf("updated name = %q", updated.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("updated live report was not received")
	}
}

func TestLivePresenceConnectionCarriesCallsInBothDirections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan livepresence.Report, 1)
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(report livepresence.Report) {
		joined <- report
	}, func(livepresence.Report) {}, func(_ context.Context, report livepresence.Report, requestID, method string, params json.RawMessage) (json.RawMessage, error) {
		if report.UUID != "session-rpc" || method != "peers.list" {
			t.Fatalf("server call = %+v %q", report, method)
		}
		if requestID == "session.inactive" {
			return nil, daemonpkg.InactiveControlError()
		}
		if requestID != "session.tool" {
			t.Fatalf("server request id = %q", requestID)
		}
		return append(json.RawMessage(nil), params...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client := livepresence.StartClient(ctx, server.listener.Addr().String(), livepresence.Report{
		UUID: "session-rpc", Name: "rpc", Product: "codex",
	}, func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "message.deliver" {
			return nil, fmt.Errorf("unexpected product callback %q", method)
		}
		return append(json.RawMessage(nil), params...), nil
	})
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("live RPC session did not join")
	}
	var fromSession json.RawMessage
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		fromSession, err = client.Call(ctx, "tool", "peers.list", map[string]string{"probe": "list_peers"})
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || !strings.Contains(string(fromSession), "list_peers") {
		t.Fatalf("session-to-daemon call = %s, %v", fromSession, err)
	}
	if _, err = client.Call(ctx, "inactive", "peers.list", map[string]any{}); err == nil {
		t.Fatal("inactive live session call succeeded")
	} else if rpcErr, ok := err.(*livepresence.RPCError); !ok || rpcErr.Code != livepresence.Unknown || string(rpcErr.Data) != `{"target":"session-rpc"}` {
		t.Fatalf("inactive live session error = %#v", err)
	}
	fromDaemon, err := server.Call(ctx, "session-rpc", "delivery", "message.deliver", map[string]string{"body": "hello"})
	if err != nil || !strings.Contains(string(fromDaemon), "hello") {
		t.Fatalf("daemon-to-session call = %s, %v", fromDaemon, err)
	}
	if _, err := server.Call(ctx, "session-rpc", "forbidden", "lane.turn.start", map[string]string{}); err == nil {
		t.Fatal("non-lane session received a lane method")
	} else if rpcErr, ok := err.(*livepresence.RPCError); !ok || rpcErr.Code != livepresence.NotPermitted || string(rpcErr.Data) != `{"method":"lane.turn.start"}` {
		t.Fatalf("non-lane method error = %#v", err)
	}
	server.mu.Lock()
	connection := server.current["session-rpc"]
	server.mu.Unlock()
	if _, err := connection.Call(ctx, "daemon.bypass", "peers.list", map[string]string{}); err == nil {
		t.Fatal("client accepted a daemon method outside its allowlist")
	} else if rpcErr, ok := err.(*livepresence.RPCError); !ok || rpcErr.Code != livepresence.NotPermitted || string(rpcErr.Data) != `{"method":"peers.list"}` {
		t.Fatalf("client allowlist error = %#v", err)
	}
}

func TestLivePresencePublishesLaneCapabilityAndServesLaneCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan livepresence.Report, 1)
	server, err := startLivePresenceServer(ctx, t.TempDir(), func(report livepresence.Report) { joined <- report }, func(livepresence.Report) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	livepresence.StartClient(ctx, server.listener.Addr().String(), livepresence.Report{
		UUID: "dsh-native", Name: "worker", Groups: []string{"team"}, Product: "dsh", Info: map[string]string{},
		Capabilities: livepresence.Capabilities{Lane: true},
	}, func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		if method == "lane.turn.start" {
			if !strings.Contains(string(params), `"input_id":"input"`) {
				t.Fatalf("lane request = %q %s", method, params)
			}
			return json.RawMessage(`{"native_message_id":"product-message"}`), nil
		}
		return json.RawMessage(`{}`), nil
	})
	select {
	case report := <-joined:
		if !report.Capabilities.Lane {
			t.Fatalf("lane capability = %#v", report.Capabilities)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lane-capable session did not join")
	}
	waitCtx, stopWait := context.WithTimeout(ctx, time.Second)
	defer stopWait()
	if err := server.Wait(waitCtx, "dsh-native", "dsh", true); err != nil {
		t.Fatal(err)
	}
	result, err := server.Call(ctx, "dsh-native", "input", "lane.turn.start", map[string]string{
		"input_id": "input", "body": "work", "mode": "followup",
	})
	if err != nil || !strings.Contains(string(result), "product-message") {
		t.Fatalf("lane response = %s, %v", result, err)
	}
	for _, method := range []string{"lane.turn.wait", "lane.turn.interrupt", "lane.session.archive"} {
		if _, err := server.Call(ctx, "dsh-native", method, method, map[string]string{}); err != nil {
			t.Fatalf("allowed %s = %v", method, err)
		}
	}
	if _, err := server.Call(ctx, "dsh-native", "forbidden", "peers.list", map[string]string{}); err == nil {
		t.Fatal("lane session received a non-session method")
	} else if rpcErr, ok := err.(*livepresence.RPCError); !ok || rpcErr.Code != livepresence.NotPermitted || string(rpcErr.Data) != `{"method":"peers.list"}` {
		t.Fatalf("lane forbidden-method error = %#v", err)
	}
}

func TestCoordinatorRebuildsLivePeerAndLaneFromReports(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parent := livepresence.Report{
		UUID: "parent", Name: "reviewer", Groups: []string{"team"}, Product: "codex",
		Info: map[string]string{"model": "native-model", "cwd": "/workspace", "future": "verbatim"},
	}
	coordinator.joinLiveSession(runtime, parent)
	attachment, active, err := runtime.Attachments().ActiveAttachment("parent")
	if err != nil || !active || attachment.Product != "codex" || !reflect.DeepEqual(attachment.Groups, []string{"team"}) ||
		!reflect.DeepEqual(attachment.Info, parent.Info) || attachment.Cwd != "" {
		t.Fatalf("reported parent = %+v, active=%v, err=%v", attachment, active, err)
	}
	lane := livepresence.Report{
		UUID: "native-lane", Name: "worker", Product: "codex",
		Groups: []string{"session:" + runtime.HostID() + "/parent", "session:" + runtime.HostID() + "/native-lane"},
	}
	coordinator.joinLiveSession(runtime, lane)
	coordinator.mu.Lock()
	actor := coordinator.lanes["native-lane"]
	cached := coordinator.laneNames["parent"]["native-lane"]
	coordinator.mu.Unlock()
	if actor == nil || actor.parentID != "parent" || actor.nativeID != "native-lane" || cached.Name != "worker" {
		t.Fatalf("reported lane actor/cache = %+v / %+v", actor, cached)
	}
	if laneAttachment, active, _ := runtime.Attachments().ActiveAttachment("native-lane"); !active || laneAttachment.State != "lane" {
		t.Fatalf("reported lane connector = %+v, active=%v", laneAttachment, active)
	}
	peers, err := runtime.Attachments().ListActive()
	if err != nil || len(peers) != 1 || peers[0].ID != "parent" {
		t.Fatalf("parent-only roster = %+v, err=%v", peers, err)
	}
	coordinator.leaveLiveSession(runtime, parent)
	coordinator.mu.Lock()
	_, cacheSurvived := coordinator.laneNames["parent"]
	coordinator.mu.Unlock()
	if cacheSurvived {
		t.Fatal("lane name cache survived its active parent")
	}
	coordinator.leaveLiveSession(runtime, lane)
	coordinator.mu.Lock()
	_, laneSurvived := coordinator.lanes["native-lane"]
	coordinator.mu.Unlock()
	if laneSurvived {
		t.Fatal("lane survived its live report connection")
	}
}

func TestUncataloguedAndLaneOnlyProductReportsRemainVisible(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	coordinator.joinLiveSession(runtime, livepresence.Report{UUID: "dsh-root", Name: "root", Product: "dsh"})
	if attachment, active, err := runtime.Attachments().ActiveAttachment("dsh-root"); err != nil || !active || attachment.Product != "dsh" {
		t.Fatalf("uncatalogued live product missing: %+v, active=%v, err=%v", attachment, active, err)
	}

	coordinator.joinLiveSession(runtime, livepresence.Report{UUID: "parent", Name: "parent", Product: "codex"})
	coordinator.joinLiveSession(runtime, livepresence.Report{
		UUID: "dsh-lane", Name: "worker", Product: "dsh",
		Groups: []string{"session:" + runtime.HostID() + "/parent"},
	})
	lane, active, err := runtime.Attachments().ActiveAttachment("dsh-lane")
	if err != nil || !active || lane.State != "lane" || lane.Product != "dsh" {
		t.Fatalf("DSH lane report = %+v, active=%v, err=%v", lane, active, err)
	}
	peers, err := runtime.Attachments().ListActive()
	if err != nil || len(peers) != 2 || peers[0].ID != "dsh-root" || peers[1].ID != "parent" {
		t.Fatalf("peer roster with lane-only reports = %+v, err=%v", peers, err)
	}
}

func TestLaneReportBindsExistingProductSessionWithoutDuplicatingActor(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parentGroups := []string{"team"}
	coordinator.joinLiveSession(runtime, livepresence.Report{UUID: "parent", Name: "parent", Product: "codex", Groups: parentGroups})
	stableGroups := []string{
		"session:" + runtime.HostID() + "/parent",
		"session:" + runtime.HostID() + "/parent/native-lane",
		"team/child",
	}
	coordinator.mu.Lock()
	coordinator.lanes["public-lane"] = &laneActor{
		id: "public-lane", product: "codex", name: "worker", parentID: "parent", state: "running",
		groups: stableGroups, done: make(chan struct{}),
	}
	coordinator.mu.Unlock()
	coordinator.joinLiveSession(runtime, livepresence.Report{
		UUID: "native-lane", Name: "worker", Product: "codex",
		Groups: []string{"team/child", "session:" + runtime.HostID() + "/parent"},
	})
	coordinator.mu.Lock()
	actor := coordinator.lanes["public-lane"]
	coordinator.mu.Unlock()
	if err := coordinator.recordLaneNativeID(runtime, actor, productruntime.NativeSessionRef{LaneID: "native-lane", NativeSessionID: "native-lane", Generation: 1}, true); err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	bound := coordinator.lanes["native-lane"]
	_, provisional := coordinator.lanes["public-lane"]
	coordinator.mu.Unlock()
	if provisional || bound != actor || actor.id != "native-lane" || actor.nativeID != "native-lane" || actor.state != "running" {
		t.Fatalf("bound actor = %+v, provisional=%v", actor, provisional)
	}
	if !reflect.DeepEqual(actor.groups, stableGroups) {
		t.Fatalf("bound actor groups = %v, want %v", actor.groups, stableGroups)
	}
	attachment, active, err := runtime.Attachments().ActiveAttachment("native-lane")
	if err != nil || !active || !reflect.DeepEqual(attachment.Groups, stableGroups) {
		t.Fatalf("bound lane attachment = %+v, active=%v, err=%v", attachment, active, err)
	}
	parent, active, err := runtime.Attachments().ActiveAttachment("parent")
	if err != nil || !active || !reflect.DeepEqual(parent.Groups, parentGroups) {
		t.Fatalf("ordinary peer attachment = %+v, active=%v, err=%v", parent, active, err)
	}
}

func TestCallerSuppliedLanePresenceBeforeOpenStillRemembersCandidate(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	coordinator.joinLiveSession(runtime, livepresence.Report{UUID: "parent", Name: "parent", Product: "codex", Groups: []string{"team"}})
	primary := "session:" + runtime.HostID() + "/parent"
	actor := &laneActor{
		id: "native-lane", product: "pi", name: "worker", parentID: "parent", state: "running",
		groups: []string{"team/child", primary, primary + "/native-lane"}, done: make(chan struct{}),
	}
	coordinator.mu.Lock()
	coordinator.lanes[actor.id] = actor
	coordinator.mu.Unlock()
	coordinator.joinLiveSession(runtime, livepresence.Report{
		UUID: "native-lane", Name: "worker", Product: "pi", Groups: []string{"team/child", primary},
	})
	if actor.nativeID != "native-lane" {
		t.Fatalf("presence did not bind caller-supplied identity: %+v", actor)
	}
	ref := productruntime.NativeSessionRef{LaneID: "native-lane", NativeSessionID: "native-lane", Generation: 1}
	if err := coordinator.recordLaneNativeID(runtime, actor, ref, true); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	want := daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "pi", Parent: "parent",
		PrimaryGroup: primary, SecondaryGroups: []string{"team/child"},
	}
	if got := before.Catalog.Lanes["native-lane"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("presence-first candidate = %+v, want %+v", got, want)
	}
	if err := coordinator.recordLaneNativeID(runtime, actor, ref, true); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Catalog.Lanes, before.Catalog.Lanes) {
		t.Fatalf("idempotent identity record changed candidate rows: before=%+v after=%+v", before.Catalog.Lanes, after.Catalog.Lanes)
	}
}

func TestRestartLeavesCandidateNonLiveUntilProductConfirmedResume(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	coordinator.joinLiveSession(runtime, livepresence.Report{UUID: "parent", Name: "parent", Product: "codex"})
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidate := daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "codex", Parent: "parent",
		PrimaryGroup: "session:" + runtime.HostID() + "/parent", SecondaryGroups: []string{"team"},
	}
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	if _, live, err := runtime.Attachments().ActiveAttachment(candidate.NativeSessionID); err != nil || live {
		t.Fatalf("candidate before product confirmation live=%v err=%v", live, err)
	}
	calls := 0
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, _ daemonpkg.ManagedAttachment, got daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		calls++
		if got.NativeSessionID != candidate.NativeSessionID {
			return laneNameEntry{}, false
		}
		return laneNameEntry{Name: "archived-worker"}, true
	}
	parent, active, err := runtime.Attachments().ActiveAttachment("parent")
	if err != nil || !active {
		t.Fatalf("parent active=%v err=%v", active, err)
	}
	actor, err := coordinator.resolveLaneActor(runtime, parent, "codex", "archived-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if actor.nativeID != "native-lane" || actor.state != "archived" || !reflect.DeepEqual(actor.explicitGroups, []string{"team"}) {
		t.Fatalf("materialized candidate = %+v", actor)
	}
	wantGroups := []string{
		"session:" + runtime.HostID() + "/parent",
		"session:" + runtime.HostID() + "/parent/native-lane",
		"team",
	}
	if !reflect.DeepEqual(actor.groups, wantGroups) {
		t.Fatalf("materialized groups = %v, want %v", actor.groups, wantGroups)
	}
	if _, live, err := runtime.Attachments().ActiveAttachment(candidate.NativeSessionID); err != nil || live {
		t.Fatalf("product-confirmed candidate was published as live: live=%v err=%v", live, err)
	}
	if _, err := coordinator.resolveLaneActor(runtime, parent, "codex", "native-lane", true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("product confirmation calls = %d, want one per explicit lookup", calls)
	}
}

func TestLaneEOFRematerializesArchivedGroupsOnlyFromDurableCandidate(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parentReport := livepresence.Report{UUID: "parent", Name: "parent", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentReport)
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidate := daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "claude", Parent: "parent",
		PrimaryGroup:    "session:" + runtime.HostID() + "/parent",
		SecondaryGroups: []string{"shared/child", "shared"},
	}
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	confirmations := 0
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, _ daemonpkg.ManagedAttachment, got daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		confirmations++
		if !reflect.DeepEqual(got, candidate) {
			t.Fatalf("candidate = %+v", got)
		}
		return laneNameEntry{Name: "worker"}, true
	}
	parent, active, err := runtime.Attachments().ActiveAttachment(parentReport.UUID)
	if err != nil || !active {
		t.Fatalf("parent active=%v err=%v", active, err)
	}
	if _, err := coordinator.resolveLaneActor(runtime, parent, candidate.Product, "worker", true); err != nil {
		t.Fatal(err)
	}
	laneReport := livepresence.Report{
		UUID: candidate.NativeSessionID, Name: "worker", Product: candidate.Product,
		Groups: candidateLaneGroups(candidate),
	}
	coordinator.joinLiveSession(runtime, laneReport)
	parentBReport := livepresence.Report{UUID: "parent-b", Name: "parent-b", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentBReport)
	parentB, active, err := runtime.Attachments().ActiveAttachment(parentBReport.UUID)
	if err != nil || !active {
		t.Fatalf("parent B active=%v err=%v", active, err)
	}
	if _, err := coordinator.resolveLaneActor(runtime, parentB, candidate.Product, "worker", true); err != nil {
		t.Fatal(err)
	}
	coordinator.leaveLiveSession(runtime, laneReport)
	coordinator.mu.Lock()
	_, actorSurvived := coordinator.lanes[candidate.NativeSessionID]
	coordinator.mu.Unlock()
	if actorSurvived {
		t.Fatal("EOF left the live lane actor")
	}

	actor, err := coordinator.resolveLaneActor(runtime, parentB, candidate.Product, "worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if confirmations != 3 || actor.parentID != parentB.ID || actor.cwd != "" || !reflect.DeepEqual(actor.explicitGroups, candidate.SecondaryGroups) || !reflect.DeepEqual(actor.groups, candidateLaneGroups(candidate)) {
		t.Fatalf("rematerialized actor=%+v confirmations=%d", actor, confirmations)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rematerialization changed durable candidate: before=%+v after=%+v", before, after)
	}
}

func TestParentPresenceEOFArchivesNonPersistentAndReleasesPersistentLane(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	driver := &parentExitLaneDriver{}
	var err error
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{"claude": driver})
	if err != nil {
		t.Fatal(err)
	}
	parentA := livepresence.Report{UUID: "parent-a", Name: "parent-a", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentA)
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	for _, nativeID := range []string{"native-idle", "native-persistent"} {
		if err := engine.Remember(daemonpkg.LaneCandidate{
			NativeSessionID: nativeID, Product: "claude", Parent: parentA.UUID,
			PrimaryGroup: "session:" + runtime.HostID() + "/" + parentA.UUID, SecondaryGroups: []string{"shared"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	idle := &laneActor{
		id: "native-idle", nativeID: "native-idle", nativeGeneration: 7, product: "claude", name: "worker-idle",
		parentID: parentA.UUID, groups: []string{"shared"}, state: "idle", done: closedLaneDone(),
	}
	persistent := &laneActor{
		id: "native-persistent", nativeID: "native-persistent", nativeGeneration: 7, product: "claude", name: "worker-persistent",
		parentID: parentA.UUID, groups: []string{"shared"}, permission: "default", persistent: true, state: "idle", done: closedLaneDone(),
	}
	coordinator.lanes[idle.id], coordinator.lanes[persistent.id] = idle, persistent
	laneReport := livepresence.Report{
		UUID: persistent.nativeID, Name: persistent.name, Product: persistent.product,
		Groups: []string{"shared", "session:" + runtime.HostID() + "/" + parentA.UUID},
	}
	coordinator.joinLiveSession(runtime, laneReport)
	if coordinator.reportedLanes[persistent.nativeID] != persistent.id {
		t.Fatalf("persistent lane report was not bound to its launched actor: %+v", coordinator.reportedLanes)
	}

	coordinator.leaveLiveSession(runtime, parentA)
	coordinator.joinLiveSession(runtime, parentA)
	coordinator.retireDepartedLiveSession(runtime, parentA)
	if idle.state != "idle" || len(driver.archives) != 0 {
		t.Fatalf("rejoined parent lost lane: actor=%+v archives=%+v", idle, driver.archives)
	}
	coordinator.leaveLiveSession(runtime, parentA)
	coordinator.retireDepartedLiveSession(runtime, parentA)
	if idle.state != "archived" || len(driver.archives) != 1 || driver.archives[0].NativeSessionID != idle.nativeID {
		t.Fatalf("nonpersistent parent exit actor=%+v archives=%+v", idle, driver.archives)
	}
	if persistent.state != "idle" || persistent.parentID != "" {
		t.Fatalf("persistent parent exit actor=%+v", persistent)
	}

	parentBReport := livepresence.Report{UUID: "parent-b", Name: "parent-b", Product: "claude", Groups: []string{"shared"}}
	coordinator.joinLiveSession(runtime, parentBReport)
	if coordinator.lanes[persistent.id] != persistent || !persistent.persistent || coordinator.reportedLanes[persistent.nativeID] != persistent.id {
		t.Fatalf("unowned persistent lane was reclassified: actor=%+v lanes=%+v reported=%+v", persistent, coordinator.lanes, coordinator.reportedLanes)
	}
	parentB, active, err := runtime.Attachments().ActiveAttachment(parentBReport.UUID)
	if err != nil || !active {
		t.Fatalf("parent B active=%v err=%v", active, err)
	}
	parentB.Cwd = t.TempDir()
	result, err := coordinator.resumeLane(context.Background(), runtime, parentB, "claude", parsedLaneCommand{target: persistent.name}, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if result["result"] != "resumed" || persistent.parentID != parentB.ID || len(driver.opens) != 1 ||
		driver.opens[0].LaneID != persistent.id || driver.opens[0].ResumeNativeID != persistent.nativeID {
		t.Fatalf("persistent reattach result=%+v actor=%+v opens=%+v", result, persistent, driver.opens)
	}
	if !persistent.autoArchive || persistent.autoArchiveDelay != defaultUnifiedLaneAutoArchiveDelay {
		t.Fatalf("resume auto-archive default = enabled:%t delay:%s", persistent.autoArchive, persistent.autoArchiveDelay)
	}
	openEnvironment := "\n" + strings.Join(driver.opens[0].Environment, "\n") + "\n"
	for _, expected := range []string{
		"\nAGENT_SESSIONS_SESSION_ID=" + persistent.id + "\n",
		"\nAGENT_SESSIONS_PRODUCT=claude\n",
		"\nAGENT_SESSIONS_SESSION_NAME=" + persistent.name + "\n",
	} {
		if !strings.Contains(openEnvironment, expected) {
			t.Fatalf("driver open environment %q lacks %q", openEnvironment, expected)
		}
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("parent exit or handover changed durable rows: before=%+v after=%+v", before, after)
	}
}

func TestOfflineLaneCandidateVisibilityAndOwnershipFollowLiveParentGroups(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		t.Fatal(err)
	}
	candidate := daemonpkg.LaneCandidate{
		NativeSessionID: "native-lane", Product: "claude", Parent: "parent-a",
		PrimaryGroup:    "session:" + runtime.HostID() + "/parent-a",
		SecondaryGroups: []string{"parent-a/child", "shared"},
	}
	if err := engine.Remember(candidate); err != nil {
		t.Fatal(err)
	}
	confirmations := 0
	coordinator.resolveCandidate = func(_ context.Context, _ *daemonpkg.Runtime, _ daemonpkg.ManagedAttachment, got daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		confirmations++
		if !reflect.DeepEqual(got, candidate) {
			t.Fatalf("candidate = %+v", got)
		}
		return laneNameEntry{Name: "shared-worker"}, true
	}
	outsider := daemonpkg.ManagedAttachment{ID: "parent-outside", Product: "claude", Cwd: t.TempDir(), Groups: []string{"outside"}}
	coordinator.liveReports[outsider.ID] = livepresence.Report{UUID: outsider.ID, Product: outsider.Product, Groups: outsider.Groups}
	if err := coordinator.ensureActiveLaneNames(context.Background(), runtime, outsider, candidate.Product); err != nil {
		t.Fatal(err)
	}
	if confirmations != 0 || len(coordinator.laneNames[outsider.ID]) != 0 {
		t.Fatalf("non-sharing parent confirmed candidate: calls=%d names=%+v", confirmations, coordinator.laneNames[outsider.ID])
	}

	parentB := daemonpkg.ManagedAttachment{ID: "parent-b", Product: "claude", Cwd: t.TempDir(), Groups: []string{"shared"}}
	coordinator.liveReports[parentB.ID] = livepresence.Report{UUID: parentB.ID, Product: parentB.Product, Groups: parentB.Groups}
	actor, err := coordinator.resolveLaneActor(runtime, parentB, candidate.Product, "shared-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if confirmations != 1 || actor.parentID != parentB.ID || !reflect.DeepEqual(actor.explicitGroups, candidate.SecondaryGroups) {
		t.Fatalf("shared candidate actor=%+v confirmations=%d", actor, confirmations)
	}
	actor.groups, err = coordinator.effectiveLaneGroups(runtime, actor, parentB)
	if err != nil {
		t.Fatal(err)
	}
	parentBAnchor := "session:" + runtime.HostID() + "/" + parentB.ID
	wantGroups := []string{"parent-a/child", parentBAnchor, parentBAnchor + "/" + candidate.NativeSessionID, "shared"}
	if !reflect.DeepEqual(actor.groups, wantGroups) {
		t.Fatalf("handed-over groups = %v, want %v", actor.groups, wantGroups)
	}
	before, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.recordLaneNativeID(runtime, actor, productruntime.NativeSessionRef{LaneID: candidate.NativeSessionID, NativeSessionID: candidate.NativeSessionID, Generation: 1}, false); err != nil {
		t.Fatalf("resume rewrote immutable candidate: %v", err)
	}
	after, err := runtime.State().Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("resume changed durable candidate: before=%+v after=%+v", before, after)
	}
}

func TestResolveLaneActorUsesExistingNativeSessionWithoutCacheDuplicate(t *testing.T) {
	runtime := newPresenceTestRuntime(t)
	coordinator := newHostCoordinator(context.Background(), t.TempDir())
	parent := daemonpkg.ManagedAttachment{ID: "parent", Product: "opencode", Cwd: "/workspace", Groups: []string{"team"}}
	coordinator.liveReports[parent.ID] = livepresence.Report{UUID: parent.ID, Product: parent.Product, Groups: parent.Groups}
	actor := &laneActor{
		id: "ses_product", nativeID: "ses_product", product: "opencode", name: "worker",
		cwd: "/workspace", parentID: parent.ID, groups: []string{"team"}, explicitGroups: []string{"team/worker"},
		state: "archived", done: closedLaneDone(),
	}
	coordinator.lanes[actor.id] = actor
	coordinator.laneNames[parent.ID] = map[string]laneNameEntry{
		actor.nativeID: {UUID: actor.nativeID, Name: actor.name, Product: actor.product},
	}

	resolved, err := coordinator.resolveLaneActor(runtime, parent, actor.product, actor.nativeID, true)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != actor || resolved.id != "ses_product" || resolved.cwd != "/workspace" || !reflect.DeepEqual(resolved.explicitGroups, []string{"team/worker"}) {
		t.Fatalf("resolved actor = %+v, want original %+v", resolved, actor)
	}
	if len(coordinator.lanes) != 1 {
		t.Fatalf("lane count = %d, want one existing actor", len(coordinator.lanes))
	}
}

func TestConnectorLiveReportUsesOnlyReportedFields(t *testing.T) {
	values := map[string]string{
		"AGENT_SESSIONS_SESSION_ID":   "session-1",
		"AGENT_SESSIONS_SESSION_NAME": "reviewer",
		"AGENT_SESSIONS_GROUPS":       `["team","team"]`,
	}
	report, ok := connectorLiveReport("claude", func(name string) string { return values[name] })
	if !ok || !reflect.DeepEqual(report, livepresence.Report{
		UUID: "session-1", Name: "reviewer", Groups: []string{"team", "team"}, Product: "claude", Info: map[string]string{},
	}) {
		t.Fatalf("connector report = %+v, ok=%v", report, ok)
	}
}

func TestConnectorLiveReportPrefersProductNativeUUID(t *testing.T) {
	values := map[string]string{
		"AGENT_SESSIONS_SESSION_ID": "public-lane",
		"CODEX_THREAD_ID":           "native-lane",
	}
	report, ok := connectorLiveReport("codex", func(name string) string { return values[name] })
	if !ok || report.UUID != "native-lane" {
		t.Fatalf("connector report = %+v, ok=%v", report, ok)
	}
}

func TestConnectorLiveReportNeverFabricatesAProductName(t *testing.T) {
	values := map[string]string{
		"CLAUDE_CODE_SESSION_ID": "native-session",
		"AGENT_SESSIONS_GROUPS":  `["team"]`,
	}
	report, ok := connectorLiveReport("claude", func(name string) string { return values[name] })
	if !ok || report.UUID != "native-session" || report.Name != "" || !reflect.DeepEqual(report.Groups, []string{"team"}) {
		t.Fatalf("connector report = %+v, ok=%v", report, ok)
	}
}

func TestClaudeActiveSessionComesFromExactNativeParentPID(t *testing.T) {
	payload := []byte(`[
		{"pid":42,"sessionId":"provisional","name":"background","kind":"background","cwd":"/wrong"},
		{"pid":7,"sessionId":"other","name":"other title","kind":"interactive","cwd":"/other"},
		{"pid":42,"sessionId":"native-session","name":"product title","kind":"interactive","cwd":"/product"}
	]`)
	base := livepresence.Report{Product: "claude", Groups: []string{"team"}, Info: map[string]string{}}
	got, ok := claudeActiveSessionForParentFromJSON(payload, 42, base)
	if !ok || got.UUID != "native-session" || got.Name != "product title" || got.Info["cwd"] != "/product" || !reflect.DeepEqual(got.Groups, []string{"team"}) {
		t.Fatalf("Claude active session = %+v, ok=%v", got, ok)
	}
	for _, invalid := range [][]byte{
		[]byte(`not-json`),
		[]byte(`[{"pid":7,"sessionId":"native-session","name":"product title","kind":"interactive"}]`),
		[]byte(`[{"pid":42,"sessionId":"one","name":"one","kind":"interactive"},{"pid":42,"sessionId":"two","name":"two","kind":"interactive"}]`),
	} {
		if got, ok := claudeActiveSessionForParentFromJSON(invalid, 42, base); ok {
			t.Fatalf("invalid native rows produced session %+v from %s", got, invalid)
		}
	}
}

func newPresenceTestRuntime(t *testing.T) *daemonpkg.Runtime {
	t.Helper()
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}
