package codex

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const (
	websocketGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxAppMessageBytes = 64 << 20
)

type webSocketTransport struct {
	connection net.Conn
	reader     *bufio.Reader
}

func dialPeerApp(ctx context.Context) (appTransport, error) {
	socket, err := appServerSocket()
	if err != nil {
		return nil, err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	transport := &webSocketTransport{connection: connection, reader: bufio.NewReader(connection)}
	if err = transport.upgrade(); err != nil {
		_ = connection.Close()
	}
	stop()
	return transport, err
}

func (w *webSocketTransport) upgrade() error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	request := strings.Join([]string{
		"GET /rpc HTTP/1.1", "Host: localhost", "Connection: Upgrade", "Upgrade: websocket",
		"Sec-WebSocket-Version: 13", "Sec-WebSocket-Key: " + key, "", "",
	}, "\r\n")
	if err := writeAll(w.connection, []byte(request)); err != nil {
		return err
	}
	response, err := http.ReadResponse(w.reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("Codex App Server rejected WebSocket upgrade: %s", response.Status)
	}
	digest := sha1.Sum([]byte(key + websocketGUID))
	if response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(digest[:]) {
		return errors.New("Codex App Server returned an invalid WebSocket accept key")
	}
	return nil
}

func (w *webSocketTransport) Write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) > maxAppMessageBytes {
		return errors.New("Codex App Server message is too large")
	}
	mask := make([]byte, 4)
	if _, err = rand.Read(mask); err != nil {
		return err
	}
	header := []byte{0x81}
	switch {
	case len(payload) < 126:
		header = append(header, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 0x80|127)
		wide := make([]byte, 8)
		binary.BigEndian.PutUint64(wide, uint64(len(payload)))
		header = append(header, wide...)
	}
	header = append(header, mask...)
	for index := range payload {
		payload[index] ^= mask[index%len(mask)]
	}
	return writeAll(w.connection, append(header, payload...))
}

func (w *webSocketTransport) Read(value any) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(w.reader, header); err != nil {
		return err
	}
	if header[0]&0x70 != 0 || header[0]&0x80 == 0 {
		return errors.New("Codex App Server returned an unsupported WebSocket frame")
	}
	opcode := header[0] & 0x0f
	if opcode == 8 {
		return io.EOF
	}
	if opcode != 1 || header[1]&0x80 != 0 {
		return errors.New("Codex App Server returned an unsupported WebSocket frame")
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		wide := make([]byte, 2)
		if _, err := io.ReadFull(w.reader, wide); err != nil {
			return err
		}
		length = uint64(binary.BigEndian.Uint16(wide))
	case 127:
		wide := make([]byte, 8)
		if _, err := io.ReadFull(w.reader, wide); err != nil {
			return err
		}
		length = binary.BigEndian.Uint64(wide)
	}
	if length > maxAppMessageBytes {
		return errors.New("Codex App Server WebSocket frame is too large")
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(w.reader, payload); err != nil {
		return err
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("malformed Codex App Server frame: %w", err)
	}
	return nil
}

func (w *webSocketTransport) Close() error { return w.connection.Close() }

func writeAll(writer io.Writer, body []byte) error {
	for len(body) != 0 {
		written, err := writer.Write(body)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
		body = body[written:]
	}
	return nil
}
