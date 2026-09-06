package codex

import (
	"bufio"
	"bytes"
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
	"testing"
)

func TestPeerWebSocketUpgradeAndFrames(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "app-server-control", "app-server-control.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("CODEX_HOME", home)
	result := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			result <- acceptErr
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		request, readErr := http.ReadRequest(reader)
		if readErr != nil {
			result <- readErr
			return
		}
		if request.URL.Path != "/rpc" || request.Header.Get("Upgrade") != "websocket" {
			result <- fmt.Errorf("upgrade request = %#v", request)
			return
		}
		digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + websocketGUID))
		_, _ = fmt.Fprintf(connection, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		payload, frameErr := readMaskedText(reader)
		if frameErr == nil && string(payload) != `{"id":1,"method":"thread/read"}` {
			frameErr = fmt.Errorf("payload = %s", payload)
		}
		if frameErr == nil {
			frameErr = writeAll(connection, append([]byte{0x81, 20}, []byte(`{"id":1,"result":{}}`)...))
		}
		if frameErr == nil {
			frameErr = writeAll(connection, []byte{0x88, 0})
		}
		result <- frameErr
	}()

	transport, err := dialPeerApp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	if err = transport.Write(map[string]any{"id": 1, "method": "thread/read"}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err = transport.Read(&response); err != nil || response.ID != 1 || string(response.Result) != `{}` {
		t.Fatalf("response = %#v, %v", response, err)
	}
	if err = transport.Read(&response); err != io.EOF {
		t.Fatalf("close = %v", err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
}

func TestPeerWebSocketCapturedInitializeFrame(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString("gX4AtXsiaWQiOjEsInJlc3VsdCI6eyJ1c2VyQWdlbnQiOiJjb2RleC10dWkvMC4xNTMuNCAoVWJ1bnR1IDI0LjQuMDsgeDg2XzY0KSB1bmtub3duIChhZ2VudGJ1cy1wcm9iZTsgMSkiLCJjb2RleEhvbWUiOiIvaG9tZS9hbnRzdC8uY29kZXgiLCJwbGF0Zm9ybUZhbWlseSI6InVuaXgiLCJwbGF0Zm9ybU9zIjoibGludXgifX0=")
	if err != nil {
		t.Fatal(err)
	}
	transport := &webSocketTransport{reader: bufio.NewReader(bytes.NewReader(raw))}
	var frame appFrame
	if err = transport.Read(&frame); err != nil || string(frame.ID) != "1" || !bytes.Contains(frame.Result, []byte(`"codexHome":"/home/antst/.codex"`)) {
		t.Fatalf("captured frame = %#v, %v", frame, err)
	}
}

func TestPeerWebSocketRejectsMaskedServerClose(t *testing.T) {
	transport := &webSocketTransport{reader: bufio.NewReader(bytes.NewReader([]byte{0x88, 0x80, 1, 2, 3, 4}))}
	if err := transport.Read(&appFrame{}); err == nil || err == io.EOF {
		t.Fatalf("masked close = %v", err)
	}
}

func readMaskedText(reader io.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	if header[0] != 0x81 || header[1]&0x80 == 0 {
		return nil, fmt.Errorf("frame header = %x", header)
	}
	length := uint64(header[1] & 0x7f)
	if length == 126 {
		wide := make([]byte, 2)
		if _, err := io.ReadFull(reader, wide); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(wide))
	} else if length == 127 {
		wide := make([]byte, 8)
		if _, err := io.ReadFull(reader, wide); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(wide)
	}
	mask, payload := make([]byte, 4), make([]byte, int(length))
	if _, err := io.ReadFull(reader, mask); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index%len(mask)]
	}
	return payload, nil
}
