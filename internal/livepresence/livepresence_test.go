package livepresence

import (
	"errors"
	"net"
	"strings"
	"testing"
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
		if got := ValidMethodResult(spec, []byte(test.body)); got != test.want {
			t.Fatalf("%s result validity = %t, want %t: %s", test.method, got, test.want, test.body)
		}
	}
	start, _ := LookupMethod("lane.turn.start")
	if !ValidMethodParams(start, []byte(`{"input_id":"i","body":"b","mode":"followup"}`)) || ValidMethodParams(start, []byte(`{"input_id":"i","body":"b"}`)) ||
		!ValidMethodResult(start, []byte(`{"native_message_id":"m"}`)) {
		t.Fatal("lane.turn.start did not use its closed param/result validators")
	}
}

func TestConnectionFrameBoundaryAndTruncation(t *testing.T) {
	bodies := [][]byte{testFrame(MaxFrameBytes, true), testFrame(MaxFrameBytes, false), testFrame(MaxFrameBytes+1, false)}
	wants := []error{nil, ErrFrameTruncated, ErrFrameTooLarge}
	for i, body := range bodies {
		server, client := net.Pipe()
		go func() { _, _ = client.Write(body); _ = client.Close() }()
		var frame Frame
		err := NewConnection(server).Decode(&frame)
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
func testFrame(size int, newline bool) []byte {
	prefix := `{"jsonrpc":"2.0","id":"x","method":"peers.list","params":{"padding":"`
	suffix := `"}}` + map[bool]string{true: "\n"}[newline]
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}
