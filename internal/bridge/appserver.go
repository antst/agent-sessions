package bridge

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
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	websocketGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxAppMessageBytes = 64 * 1024 * 1024
)

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "App Server request failed"
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("App Server request failed (%d)", e.Code)
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

type rpcNotification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

type serverRequestHandler func(*appServerClient, rpcServerRequest)

type appServerClient struct {
	conn          net.Conn
	read          *bufio.Reader
	peerPID       int
	peerProcStart string

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcReply

	notifications chan rpcNotification
	serverHandler serverRequestHandler
	done          chan struct{}
	closeOnce     sync.Once
	closeErr      error
}

func dialAppServer(ctx context.Context, socketPath string) (*appServerClient, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	peerPID, err := unixPeerPID(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("corroborate App Server socket peer: %w", err)
	}
	peerProcStart, err := captureProcessStart(peerPID)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("capture App Server process identity: %w", err)
	}
	client := &appServerClient{
		conn: conn, read: bufio.NewReader(conn), nextID: 1,
		peerPID: peerPID, peerProcStart: peerProcStart,
		pending: map[int64]chan rpcReply{}, notifications: make(chan rpcNotification, 256),
		serverHandler: handleNativeServerRequest, done: make(chan struct{}),
	}
	if err := client.upgrade(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go client.readLoop()
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var initialized map[string]any
	if err := client.request(initCtx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name": "claude_code_peer_native", "title": "Claude Code Peer Bridge", "version": "0.3.0",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		client.close()
		return nil, err
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func (c *appServerClient) upgrade(ctx context.Context) error {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := strings.Join([]string{
		"GET /rpc HTTP/1.1",
		"Host: localhost",
		"Connection: Upgrade",
		"Upgrade: websocket",
		"Sec-WebSocket-Version: 13",
		"Sec-WebSocket-Key: " + key,
		"", "",
	}, "\r\n")
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = c.conn.SetDeadline(deadline)
	if _, err := io.WriteString(c.conn, request); err != nil {
		return err
	}
	response, err := http.ReadResponse(c.read, &http.Request{Method: http.MethodGet})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("app server rejected WebSocket upgrade: %s", response.Status)
	}
	digest := sha1.Sum([]byte(key + websocketGUID))
	expected := base64.StdEncoding.EncodeToString(digest[:])
	if response.Header.Get("Sec-WebSocket-Accept") != expected {
		return errors.New("app server returned an invalid WebSocket accept key")
	}
	_ = c.conn.SetDeadline(time.Time{})
	return nil
}

func (c *appServerClient) request(ctx context.Context, method string, params any, output any) error {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	result := make(chan rpcReply, 1)
	c.pending[id] = result
	c.mu.Unlock()

	body, err := json.Marshal(map[string]any{"method": method, "id": id, "params": params})
	if err == nil {
		err = c.writeFrame(1, body)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case reply := <-result:
		if reply.err != nil {
			return reply.err
		}
		if output == nil || len(reply.result) == 0 || string(reply.result) == "null" {
			return nil
		}
		return json.Unmarshal(reply.result, output)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		return c.closeReason()
	}
}

func (c *appServerClient) notify(method string, params any) error {
	body, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return err
	}
	return c.writeFrame(1, body)
}

func (c *appServerClient) respond(id json.RawMessage, result any, rpcErr *rpcError) error {
	body := map[string]any{"id": id}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.writeFrame(1, encoded)
}

func (c *appServerClient) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.closeReason()
	default:
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	lengthByte := byte(0x80)
	var extended []byte
	switch {
	case len(payload) < 126:
		lengthByte |= byte(len(payload)) // #nosec G115 -- This branch proves the value is below 126.
	case len(payload) <= 0xffff:
		lengthByte |= 126
		wide := make([]byte, 2)
		binary.BigEndian.PutUint16(wide, uint16(len(payload))) //nolint:gosec // This branch bounds payload at 0xffff.
		extended = wide
	default:
		lengthByte |= 127
		wide := make([]byte, 8)
		binary.BigEndian.PutUint64(wide, uint64(len(payload)))
		extended = wide
	}
	frameHeader := append([]byte{0x80 | opcode, lengthByte}, extended...)
	frameHeader = append(frameHeader, mask...)
	encoded := make([]byte, len(payload))
	for i := range payload {
		encoded[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(frameHeader); err != nil {
		return err
	}
	_, err := c.conn.Write(encoded)
	return err
}

func (c *appServerClient) readLoop() {
	var fragments []byte
	var fragmentOpcode byte
	for {
		final, opcode, payload, err := c.readFrame()
		if err != nil {
			c.closeWithError(err)
			return
		}
		switch opcode {
		case 8:
			c.closeWithError(errors.New("app server closed the WebSocket"))
			return
		case 9:
			_ = c.writeFrame(10, payload)
			continue
		case 1, 2:
			fragments = append(fragments[:0], payload...)
			fragmentOpcode = opcode
		case 0:
			if fragmentOpcode == 0 {
				c.closeWithError(errors.New("unexpected WebSocket continuation frame"))
				return
			}
			fragments = append(fragments, payload...)
		default:
			continue
		}
		if len(fragments) > maxAppMessageBytes {
			c.closeWithError(errors.New("app server WebSocket message is too large"))
			return
		}
		if !final {
			continue
		}
		if fragmentOpcode == 1 {
			c.handleMessage(fragments)
		}
		fragments = nil
		fragmentOpcode = 0
	}
}

func (c *appServerClient) readFrame() (bool, byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.read, header); err != nil {
		return false, 0, nil, err
	}
	final := header[0]&0x80 != 0
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		wide := make([]byte, 2)
		if _, err := io.ReadFull(c.read, wide); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(wide))
	case 127:
		wide := make([]byte, 8)
		if _, err := io.ReadFull(c.read, wide); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(wide)
	}
	if length > maxAppMessageBytes {
		return false, 0, nil, errors.New("app server WebSocket frame is too large")
	}
	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(c.read, mask); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.read, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		if masked {
			payload[i] ^= mask[i%4]
		}
	}
	return final, opcode, payload, nil
}

func (c *appServerClient) handleMessage(body []byte) {
	var message struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Params json.RawMessage  `json:"params"`
		Result json.RawMessage  `json:"result"`
		Error  *rpcError        `json:"error"`
	}
	if json.Unmarshal(body, &message) != nil {
		return
	}
	if message.ID != nil && message.Method != "" {
		if c.serverHandler != nil {
			request := rpcServerRequest{ID: append(json.RawMessage(nil), (*message.ID)...), Method: message.Method, Params: append(json.RawMessage(nil), message.Params...)}
			go c.serverHandler(c, request)
		} else {
			_ = c.respond(*message.ID, nil, &rpcError{Code: -32601, Message: "unsupported App Server request: " + message.Method})
		}
		return
	}
	if message.ID != nil {
		var id int64
		if json.Unmarshal(*message.ID, &id) != nil {
			return
		}
		c.mu.Lock()
		waiting := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if waiting != nil {
			if message.Error != nil {
				waiting <- rpcReply{err: message.Error}
			} else {
				waiting <- rpcReply{result: message.Result}
			}
		}
		return
	}
	if message.Method != "" {
		select {
		case c.notifications <- rpcNotification{Method: message.Method, Params: message.Params}:
		case <-c.done:
		}
	}
}

func (c *appServerClient) closeWithError(err error) {
	c.closeOnce.Do(func() {
		if err == nil {
			err = errors.New("app server connection closed")
		}
		c.mu.Lock()
		c.closeErr = err
		pending := c.pending
		c.pending = map[int64]chan rpcReply{}
		c.mu.Unlock()
		_ = c.conn.Close()
		close(c.done)
		for _, waiting := range pending {
			waiting <- rpcReply{err: err}
		}
	})
}

func (c *appServerClient) closeReason() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	return errors.New("app server connection closed")
}

func (c *appServerClient) close() { c.closeWithError(errors.New("app server client closed")) }

func requestWithTimeout(client *appServerClient, timeout time.Duration, method string, params any, output any) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.request(ctx, method, params, output)
}

func appServerSocketRunning(socketPath string) (bool, error) {
	connection, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return false, nil
	}
	return false, err
}

func activeAppServerThreads(client *appServerClient) ([]string, error) {
	loaded, err := loadedPreparedThreads(client)
	if err != nil {
		return nil, err
	}
	active := make([]string, 0)
	for threadID := range loaded {
		thread, err := readExactPreparedThread(client, threadID)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", threadID, err)
		}
		if statusType(thread.Status) == "active" {
			active = append(active, threadID)
		}
	}
	sort.Strings(active)
	return active, nil
}
