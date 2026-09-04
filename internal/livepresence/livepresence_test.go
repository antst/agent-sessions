package livepresence

import (
	"errors"
	"net"
	"strings"
	"testing"
)

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
