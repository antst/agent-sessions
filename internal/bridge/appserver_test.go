package bridge

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

func startFakeNativeAppServerAt(t *testing.T, socket string, handler func(map[string]any) (any, error)) *fakeAppServer {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAppServer{
		listener: listener,
		requests: make(chan map[string]any, 64),
		handler:  handler,
		done:     make(chan struct{}),
	}
	go fake.accept()
	t.Cleanup(fake.close)
	return fake
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
		encoded, _ := json.Marshal(response)
		if writeTestFrame(conn, encoded) != nil {
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestNativeAppServerClientRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "app-server.sock")
	startFakeNativeAppServerAt(t, socket, func(request map[string]any) (any, error) {
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
