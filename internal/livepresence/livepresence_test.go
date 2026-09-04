package livepresence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/testutil"
)

func TestClosedLaneMethodValidators(t *testing.T) {
	doctor := `{"type":"lane.doctor","contract_version":2,"authority":"daemon","product":"codex","ready":true,"native_path":"/bin/codex","runtime_path":"/bin/codex","daemon_reachable":true,"supervisor_reachable":true,"codex_available":true,"codex_path":"/bin/codex","codex_version":"1"}`
	list := `{"type":"lane.list","product":"codex","lanes":[{"type":"lane.status","product":"codex","session_id":"s","name":"n","cwd":"/w","groups":[],"permission_mode":"default","state":"idle","turn_id":"t","outcome":"completed","exit":0,"owner_session_id":"p","persistent":false,"auto_archive":true,"auto_archive_after_seconds":1,"auto_archive_at":0}]}`
	for _, test := range []struct {
		method, body string
		want         bool
	}{
		{"lane.doctor", doctor, true}, {"lane.doctor", strings.Replace(doctor, `"codex_path"`, `"qwen_path"`, 1), false},
		{"lane.doctor", strings.Replace(doctor, `"ready":true`, `"ready":"yes"`, 1), false}, {"lane.doctor", strings.TrimSuffix(doctor, "}") + `,"extra":true}`, false},
		{"lane.list", list, true}, {"lane.list", strings.Replace(list, `"state":"idle"`, `"state":1`, 1), false},
		{"lane.list", strings.Replace(list, `"state":"idle"`, `"state":"idle","extra":true`, 1), false},
	} {
		spec, _ := LookupMethod(test.method)
		if got := ValidMethodResult(spec, []byte(`{"product":"codex","arguments":[]}`), []byte(test.body)); got != test.want {
			t.Fatalf("%s result validity = %t, want %t: %s", test.method, got, test.want, test.body)
		}
	}
	start, _ := LookupMethod("lane.turn.start")
	if !ValidMethodParams(start, []byte(`{"input_id":"i","body":"b","mode":"followup"}`)) || ValidMethodParams(start, []byte(`{"input_id":"i","body":"b"}`)) ||
		!ValidMethodResult(start, nil, []byte(`{"native_message_id":"m"}`)) {
		t.Fatal("lane.turn.start did not use its closed param/result validators")
	}
}

func TestMethodAuthorityInventoryAndClosedLaneResults(t *testing.T) {
	type authority struct {
		direction          MethodDirection
		lane, input, first bool
		result             byte
	}
	want := map[string]authority{
		"session.hello": {ClientToDaemon, false, false, true, 'e'}, "session.update": {ClientToDaemon, false, false, false, 'e'},
		"session.superseded": {DaemonToClient, false, false, false, 'e'},
		"peers.list":         {ClientToDaemon, false, false, false, 'p'}, "message.send": {ClientToDaemon, false, false, false, 'm'},
		"lane.doctor": {ClientToDaemon, true, false, false, 'd'}, "lane.list": {ClientToDaemon, true, false, false, 'l'},
		"lane.start": {ClientToDaemon, true, true, false, 'r'}, "lane.run": {ClientToDaemon, true, true, false, 'c'}, "lane.resume": {ClientToDaemon, true, true, false, 'c'}, "lane.steer": {ClientToDaemon, true, true, false, 's'},
		"lane.wait": {ClientToDaemon, true, false, false, 'c'}, "lane.status": {ClientToDaemon, true, false, false, 't'}, "lane.interrupt": {ClientToDaemon, true, false, false, 'i'}, "lane.archive": {ClientToDaemon, true, false, false, 'a'},
		"message.deliver": {DaemonToClient, false, false, false, 'e'}, "lane.turn.start": {DaemonToClient, true, false, false, 'n'}, "lane.turn.wait": {DaemonToClient, true, false, false, 'w'},
		"lane.turn.interrupt": {DaemonToClient, true, false, false, 'e'}, "lane.session.archive": {DaemonToClient, true, false, false, 'e'},
	}
	if len(methodTable) != len(want) {
		t.Fatalf("method authority count = %d, want %d", len(methodTable), len(want))
	}
	for name, expected := range want {
		spec, ok := LookupMethod(name)
		if !ok || spec.Direction != expected.direction || spec.Lane != expected.lane || spec.NeedsInput != expected.input || spec.First != expected.first || spec.params == nil || spec.result != expected.result {
			t.Fatalf("method %s = %+v, found=%t, want %+v", name, spec, ok, expected)
		}
	}
	if _, ok := LookupMethod("lane.future"); ok {
		t.Fatal("unknown method entered the closed authority")
	}
	superseded, _ := LookupMethod("session.superseded")
	if !ValidMethodParams(superseded, []byte(`{}`)) || ValidMethodParams(superseded, []byte(`{"extra":true}`)) ||
		!ValidMethodResult(superseded, []byte(`{}`), []byte(`{}`)) {
		t.Fatal("session.superseded is not closed {} to {}")
	}

	status := `{"type":"lane.status","product":"codex","session_id":"s","name":"n","cwd":"/w","groups":[],"permission_mode":"default","state":"idle","turn_id":"","outcome":"","exit":null,"owner_session_id":"p","persistent":false,"auto_archive":true,"auto_archive_after_seconds":1.5,"auto_archive_at":0}`
	ready := strings.Replace(status, `"type":"lane.status"`, `"type":"lane.ready"`, 1)
	ready = strings.TrimSuffix(ready, "}") + `,"contract_version":2}`
	completed := `{"type":"turn.completed","product":"codex","session_id":"s","turn_id":"t","status":"completed","outcome":"completed","exit":0,"result":"done","diagnostic":""}`
	results := map[string]string{
		"lane.doctor": `{"type":"lane.doctor","contract_version":2,"authority":"daemon","product":"codex","ready":true,"native_path":"/bin/codex","runtime_path":"/bin/codex","daemon_reachable":true,"supervisor_reachable":true,"codex_available":true,"codex_path":"/bin/codex","codex_version":"1"}`,
		"lane.list":   `{"type":"lane.list","product":"codex","lanes":[` + status + `]}`, "lane.start": ready,
		"lane.run": completed, "lane.resume": completed, "lane.wait": completed,
		"lane.steer":  `{"type":"turn.steered","session_id":"s","turn_id":"t","native_message_id":"m"}`,
		"lane.status": status, "lane.interrupt": `{"type":"turn.interrupting","session_id":"s","turn_id":"t"}`,
		"lane.archive": `{"type":"lane.archived","product":"codex","session_id":"s","name":"n","already_archived":false}`,
	}
	params := json.RawMessage(`{"product":"codex","arguments":[]}`)
	for method, body := range results {
		spec, _ := LookupMethod(method)
		if !ValidMethodResult(spec, params, []byte(body)) {
			t.Fatalf("valid %s result rejected: %s", method, body)
		}
		var value map[string]any
		_ = json.Unmarshal([]byte(body), &value)
		for name, mutate := range map[string]func(map[string]any){
			"missing": func(v map[string]any) { delete(v, "type") }, "extra": func(v map[string]any) { v["type product"] = true }, "wrong-type": func(v map[string]any) { v["type"] = 1 },
		} {
			copy := mapsClone(value)
			mutate(copy)
			encoded, _ := json.Marshal(copy)
			if ValidMethodResult(spec, params, encoded) {
				t.Fatalf("%s %s result accepted: %s", method, name, encoded)
			}
		}
		if ValidMethodResult(spec, params, []byte(`null`)) {
			t.Fatalf("%s accepted null result", method)
		}
		if _, bound := value["product"]; bound && ValidMethodResult(spec, []byte(`{"product":"qwen","arguments":[]}`), []byte(body)) {
			t.Fatalf("%s accepted result for the wrong requested product", method)
		}
	}
	for _, method := range []string{"lane.run", "lane.resume", "lane.wait"} {
		spec, _ := LookupMethod(method)
		for suffix, accepted := range map[string]bool{"": true, `,"native_stop_reason":"aborted"`: true, `,"native_stop_reason":""`: false, `,"native_stop_reason":1`: false} {
			body := strings.TrimSuffix(completed, "}") + suffix + "}"
			if got := ValidMethodResult(spec, params, []byte(body)); got != accepted {
				t.Fatalf("%s native reason validity = %t, want %t: %s", method, got, accepted, body)
			}
		}
	}
	for _, method := range []string{"lane.start", "lane.steer", "lane.status", "lane.interrupt", "lane.archive"} {
		spec, _ := LookupMethod(method)
		body := strings.TrimSuffix(results[method], "}") + `,"native_stop_reason":"aborted"}`
		if ValidMethodResult(spec, params, []byte(body)) {
			t.Fatalf("%s accepted native_stop_reason", method)
		}
	}
}

func TestSequencedDaemonRequestsDoNotCollideAndPendingFailsOnce(t *testing.T) {
	local, remote := net.Pipe()
	rpc := NewConnection(local)
	callDone := make(chan error, 1)
	go func() {
		_, err := rpc.CallNext(context.Background(), "session.superseded", struct{}{})
		callDone <- err
	}()
	decoder := json.NewDecoder(remote)
	var first, second Frame
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	sendDone := make(chan error, 1)
	go func() { sendDone <- rpc.SendNext("session.superseded", struct{}{}) }()
	if err := decoder.Decode(&second); err != nil || string(first.ID) == string(second.ID) {
		t.Fatalf("sequenced requests = %+v / %+v, err=%v", first, second, err)
	}
	rpc.Fail()
	_ = local.Close()
	_ = remote.Close()
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	if err := <-callDone; err == nil {
		t.Fatal("pending call survived terminal supersession write")
	}
}

func TestClientTreatsSupersessionAsTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(testutil.ShortSocketRoot(t, "sup-", "presence.sock"), "presence.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	served := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			served <- acceptErr
			return
		}
		rpc := NewConnection(connection)
		var hello Frame
		if err := rpc.Decode(&hello); err == nil {
			err = rpc.Write(Success(hello.ID, json.RawMessage(`{}`)))
		}
		if err == nil {
			err = rpc.SendNext("session.superseded", struct{}{})
		}
		_ = connection.Close()
		served <- err
	}()
	client := StartClient(ctx, path, Report{UUID: "same", Name: "old", Product: "future", Groups: []string{}, Info: map[string]string{}}, nil)
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("superseded client did not terminate")
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, "late", "peers.list", map[string]any{}); err == nil {
		t.Fatalf("terminal client call error = %v", err)
	}
	if unix, ok := listener.(*net.UnixListener); ok {
		_ = unix.SetDeadline(time.Now().Add(50 * time.Millisecond))
		if connection, err := unix.Accept(); err == nil {
			_ = connection.Close()
			t.Fatal("superseded client reconnected")
		}
	}
}

func mapsClone(value map[string]any) map[string]any {
	clone := make(map[string]any, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}

func TestAppendixWireGrammarAndRequiredDeliveryFields(t *testing.T) {
	hello := func(protocol, uuid, capabilities string) json.RawMessage {
		optional := ""
		if capabilities != "" {
			optional = `,"capabilities":` + capabilities
		}
		return json.RawMessage(fmt.Sprintf(`{"protocol":%s,"uuid":%q,"name":"","groups":[],"product":"codex","info":{}%s}`, protocol, uuid, optional))
	}
	for _, test := range []struct {
		name, protocol, uuid, capabilities string
		want                               bool
	}{
		{"integer", "1", "native", "", true}, {"decimal", "1.0", "native", "", true}, {"exponent", "1e0", "native", "", true},
		{"safe-max", "9007199254740991", "native", "", true}, {"too-large", "9007199254740992", "native", "", false}, {"fraction", "1.5", "native", "", false},
		{"capability", "1", "native", `{"lane":true}`, true}, {"capability-null", "1", "native", `null`, false}, {"capability-empty", "1", "native", `{}`, false}, {"capability-false", "1", "native", `{"lane":false}`, false}, {"capability-extra", "1", "native", `{"lane":true,"extra":true}`, false},
		{"bom-native-id", "1", "native\ufeffid", "", true}, {"space-native-id", "1", "native id", "", false}, {"slash-native-id", "1", "native/id", "", false}, {"control-native-id", "1", "native\n", "", false},
		{"native-id-max", "1", strings.Repeat("x", 128), "", true}, {"native-id-over", "1", strings.Repeat("x", 129), "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := DecodeHello(hello(test.protocol, test.uuid, test.capabilities))
			if got := err == nil; got != test.want {
				t.Fatalf("hello validity = %t, want %t: %v", got, test.want, err)
			}
		})
	}
	for _, test := range []struct {
		name string
		want bool
	}{{"", true}, {strings.Repeat("n", 256), true}, {strings.Repeat("n", 257), false}, {"bad\u007fname", false}} {
		raw := json.RawMessage(fmt.Sprintf(`{"protocol":1,"uuid":"native","name":%q,"groups":[],"product":"codex","info":{}}`, test.name))
		if _, _, err := DecodeHello(raw); (err == nil) != test.want {
			t.Fatalf("hello name validity = %t, want %t: %q", err == nil, test.want, test.name)
		}
		update := json.RawMessage(fmt.Sprintf(`{"name":%q,"info":{}}`, test.name))
		if _, _, err := DecodeUpdate(update); (err == nil) != test.want {
			t.Fatalf("update name validity = %t, want %t: %q", err == nil, test.want, test.name)
		}
	}
	for _, test := range []struct {
		info string
		want bool
	}{{`{}`, true}, {`{"cwd":"/work"}`, true}, {`{"cwd":""}`, false}, {`{"cwd":"work"}`, false}} {
		raw := json.RawMessage(`{"protocol":1,"uuid":"native","name":"","groups":[],"product":"codex","info":` + test.info + `}`)
		if _, _, err := DecodeHello(raw); (err == nil) != test.want {
			t.Fatalf("hello info validity = %t, want %t: %s", err == nil, test.want, test.info)
		}
		if _, _, err := DecodeUpdate(json.RawMessage(`{"name":"","info":` + test.info + `}`)); (err == nil) != test.want {
			t.Fatalf("update info validity = %t, want %t: %s", err == nil, test.want, test.info)
		}
	}
	for _, test := range []struct {
		name, body string
		want       bool
	}{
		{"lane-arguments-any-string", `{"product":"codex","arguments":["line\nbreak","\u0000"],"input":" "}`, true},
		{"lane-opaque-product", `{"product":"future-product","arguments":[],"input":"work"}`, true},
		{"lane-absolute-cwd", `{"product":"codex","arguments":[],"input":"work","cwd":"/work","host":" "}`, true},
		{"lane-relative-cwd", `{"product":"codex","arguments":[],"input":"work","cwd":"work"}`, false},
		{"lane-null-cwd", `{"product":"codex","arguments":[],"input":"work","cwd":null}`, false},
		{"lane-null-host", `{"product":"codex","arguments":[],"input":"work","host":null}`, false},
		{"lane-empty-input", `{"product":"codex","arguments":[],"input":""}`, false}, {"lane-nonstring-argument", `{"product":"codex","arguments":[1],"input":"work"}`, false},
	} {
		spec, _ := LookupMethod("lane.start")
		if got := ValidMethodParams(spec, []byte(test.body)); got != test.want {
			t.Fatalf("%s validity = %t, want %t", test.name, got, test.want)
		}
	}
	status, _ := LookupMethod("lane.status")
	if ValidMethodParams(status, []byte(`{"product":"codex","arguments":[],"input":null}`)) {
		t.Fatal("no-input lane method accepted explicit null input")
	}
	for _, test := range []struct {
		method, body string
		want         bool
	}{
		{"lane.turn.start", `{"input_id":" ","body":"\u0000","mode":"followup"}`, true}, {"lane.turn.start", `{"input_id":"","body":"x","mode":"followup"}`, false},
		{"lane.turn.wait", `{"native_message_id":"\n"}`, true}, {"lane.turn.wait", `{"native_message_id":""}`, false},
	} {
		spec, _ := LookupMethod(test.method)
		if got := ValidMethodParams(spec, []byte(test.body)); got != test.want {
			t.Fatalf("%s params validity = %t, want %t: %s", test.method, got, test.want, test.body)
		}
	}
	base := `{"message_id":" ","from":{"uuid":"parent","name":"","product":"codex","groups":[]},"body":""}`
	if _, err := DecodeDeliver([]byte(base)); err != nil {
		t.Fatalf("empty required delivery strings rejected: %v", err)
	}
	for _, body := range []string{strings.Replace(base, `,"body":""`, "", 1), strings.Replace(base, `"name":"",`, "", 1)} {
		if _, err := DecodeDeliver([]byte(body)); err == nil {
			t.Fatalf("delivery with omitted required field accepted: %s", body)
		}
	}
}

func TestConnectionEnforcesFirstFrameAndNativeLaneAuthority(t *testing.T) {
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":"x","method":"peers.list","params":{},"error":null}`,
		`{"jsonrpc":"2.0","id":"x","result":{},"error":null}`,
		`{"jsonrpc":"2.0","id":"x","method":null,"params":{}}`,
		`{"jsonrpc":"2.0","id":"x","method":null,"result":{}}`,
	} {
		var frame Frame
		if err := DecodeStrict([]byte(raw), &frame); err != nil || ValidFrame(frame) {
			t.Fatalf("explicit null envelope field was not preserved: frame=%+v err=%v", frame, err)
		}
	}
	decode := func(t *testing.T, reported bool, request Frame) error {
		server, client := net.Pipe()
		defer server.Close()
		go func() { _ = NewConnection(client).Write(request); _ = client.Close() }()
		rpc := NewConnection(server)
		if reported {
			rpc.SetReport(Report{UUID: "native", Name: "", Product: "codex", Groups: []string{}, Info: map[string]string{}})
		}
		return rpc.Decode(&Frame{})
	}
	empty, hello := json.RawMessage(`{}`), helloParams("native", false)
	if err := decode(t, false, Frame{JSONRPC: "2.0", ID: []byte(`"x"`), Method: "peers.list", Params: empty}); err == nil {
		t.Fatal("non-hello first frame accepted")
	}
	if err := decode(t, true, Frame{JSONRPC: "2.0", ID: []byte(`"x"`), Method: "session.hello", Params: hello}); err == nil {
		t.Fatal("later session.hello accepted")
	}
	server, daemon := net.Pipe()
	caller, peer := NewConnection(server), NewConnection(daemon)
	peer.SetReport(Report{UUID: "native", Name: "", Product: "codex", Groups: []string{}, Info: map[string]string{}})
	callDone := make(chan error, 1)
	go func() {
		_, err := caller.Call(context.Background(), "invalid-result", "peers.list", map[string]any{})
		callDone <- err
	}()
	var request, response Frame
	if err := peer.Decode(&request); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- peer.Write(Success(request.ID, json.RawMessage(`null`))) }()
	if err := caller.Decode(&response); err != nil || !caller.Resolve(response) {
		t.Fatalf("resolve invalid result = %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-callDone; err == nil {
		t.Fatal("Connection.Call accepted a null result")
	}
	_ = server.Close()
	_ = daemon.Close()

	for _, test := range []struct {
		name        string
		capable     bool
		params      string
		code, calls int
	}{
		{"not-capable", false, `{"input_id":"i","body":"b","mode":"followup"}`, NotPermitted, 0},
		{"malformed", true, `{"input_id":"i"}`, InvalidParams, 0}, {"valid", true, `{"input_id":"i","body":"b","mode":"followup"}`, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, product := net.Pipe()
			defer server.Close()
			rpc := NewConnection(product)
			rpc.SetReport(Report{UUID: "native", Name: "", Product: "codex", Groups: []string{}, Info: map[string]string{}, Capabilities: Capabilities{Lane: test.capable}})
			calls := 0
			client := &Client{call: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`{"native_message_id":"m"}`), nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go client.read(ctx, rpc)
			peer := NewConnection(server)
			request := Frame{JSONRPC: "2.0", ID: []byte(`"call"`), Method: "lane.turn.start", Params: []byte(test.params)}
			if err := peer.Write(request); err != nil {
				t.Fatal(err)
			}
			var response Frame
			_ = server.SetReadDeadline(time.Now().Add(time.Second))
			if err := peer.Decode(&response); err != nil {
				t.Fatal(err)
			}
			if calls != test.calls || test.code == 0 && response.Error != nil || test.code != 0 && (response.Error == nil || response.Error.Code != test.code) {
				t.Fatalf("response/callbacks = %+v/%d, want code/calls %d/%d", response, calls, test.code, test.calls)
			}
		})
	}
}

func helloParams(uuid string, lane bool) json.RawMessage {
	capability := ""
	if lane {
		capability = `,"capabilities":{"lane":true}`
	}
	return json.RawMessage(fmt.Sprintf(`{"protocol":1,"uuid":%q,"name":"","groups":[],"product":"codex","info":{}%s}`, uuid, capability))
}

func TestConnectionFrameBoundaryAndTruncation(t *testing.T) {
	bodies := [][]byte{testFrame(MaxFrameBytes, true), testFrame(MaxFrameBytes, false), testFrame(MaxFrameBytes+1, false)}
	wants := []error{nil, ErrFrameTruncated, ErrFrameTooLarge}
	for i, body := range bodies {
		server, client := net.Pipe()
		go func() { _, _ = client.Write(body); _ = client.Close() }()
		var frame Frame
		rpc := NewConnection(server)
		rpc.SetReport(Report{UUID: "native", Name: "native", Product: "codex", Groups: []string{}, Info: map[string]string{}})
		err := rpc.Decode(&frame)
		_ = server.Close()
		if !errors.Is(err, wants[i]) || wants[i] == nil && frame.Method != "peers.list" {
			t.Fatalf("case %d: frame=%+v error=%v want=%v", i, frame, err, wants[i])
		}
	}
	server, client := net.Pipe()
	_ = client.Close()
	rpc := NewConnection(server)
	var observed []Frame
	rpc.Observe(func(direction string, frame Frame) { observed = append(observed, frame) })
	frame := Frame{JSONRPC: "2.0", ID: []byte(`"attempt"`), Method: "peers.list", Params: []byte(`{}`)}
	if err := rpc.Write(frame); err == nil || len(observed) != 1 {
		t.Fatalf("write error = %v, observations = %+v", err, observed)
	}
	frame.Params[0] = '['
	if string(observed[0].Params) != `{}` {
		t.Fatalf("observation was not cloned: %+v", observed[0])
	}
}

func TestClientBeforePublishGatesTheCurrentConnection(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "reject"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(testutil.ShortSocketRoot(t, "lp-", "presence.sock"), "presence.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			accepted := make(chan struct{}, 2)
			go func() {
				attempts := 1
				if fail {
					attempts++
				}
				for range attempts {
					connection, acceptErr := listener.Accept()
					if acceptErr != nil {
						return
					}
					accepted <- struct{}{}
					rpc := NewConnection(connection)
					var hello Frame
					if rpc.Decode(&hello) == nil {
						_ = rpc.Write(Frame{JSONRPC: "2.0", ID: hello.ID, Result: json.RawMessage(`{}`)})
					}
					_ = connection.Close()
				}
			}()
			entered, release := make(chan struct{}, 2), make(chan struct{})
			client := StartClientWithOptions(ctx, path, Report{UUID: "native", Name: "native", Product: "codex", Groups: []string{}, Info: map[string]string{}}, nil, nil, ClientOptions{
				BeforePublish: func(context.Context) error {
					entered <- struct{}{}
					<-release
					if fail {
						return errors.New("stale image")
					}
					return nil
				},
			})
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("before-publish gate was not called")
			}
			<-accepted
			if _, err := client.Call(ctx, "gated", "peers.list", map[string]any{}); err == nil {
				t.Fatal("connection was published while the gate was blocked")
			}
			close(release)
			if fail {
				select {
				case <-client.Ready():
					t.Fatal("rejected connection became ready")
				case <-time.After(50 * time.Millisecond):
				}
				select {
				case <-accepted:
				case <-time.After(3 * time.Second):
					t.Fatal("rejected connection did not enter the existing reconnect loop")
				}
				return
			}
			select {
			case <-client.Ready():
			case <-time.After(time.Second):
				t.Fatal("accepted connection was not published")
			}
		})
	}
}

func TestClientBeforePublishDoesNotOverwriteAConcurrentReportUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(testutil.ShortSocketRoot(t, "lp-", "presence.sock"), "presence.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	disconnected := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer close(disconnected)
		rpc := NewConnection(connection)
		var hello Frame
		if rpc.Decode(&hello) == nil {
			_ = rpc.Write(Frame{JSONRPC: "2.0", ID: hello.ID, Result: json.RawMessage(`{}`)})
		}
		_, _ = io.Copy(io.Discard, connection)
	}()
	entered, release := make(chan struct{}), make(chan struct{})
	client := StartClientWithOptions(ctx, path, Report{UUID: "native", Name: "before", Product: "codex", Groups: []string{}, Info: map[string]string{}}, nil, nil, ClientOptions{
		BeforePublish: func(context.Context) error { close(entered); <-release; return nil },
	})
	<-entered
	if err := client.UpdateReport(ctx, Report{UUID: "native", Name: "after", Product: "codex", Groups: []string{}, Info: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("stale hello connection was not rejected")
	}
	client.mu.Lock()
	name, current := client.report.Name, client.current
	client.mu.Unlock()
	if name != "after" || current != nil {
		t.Fatalf("report/current after blocked hook = %q/%v", name, current)
	}
	select {
	case <-client.Ready():
		t.Fatal("stale report became ready")
	default:
	}
}
func testFrame(size int, newline bool) []byte {
	prefix := `{"jsonrpc":"2.0","id":"x","method":"peers.list","params":{"padding":"`
	suffix := `"}}` + map[bool]string{true: "\n"}[newline]
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}
