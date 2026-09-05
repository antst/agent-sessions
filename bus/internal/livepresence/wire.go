package livepresence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
)

const MaxFrameBytes = 1 << 20

var (
	ErrFrameTooLarge    = errors.New("session frame exceeds 1 MiB")
	ErrFrameTruncated   = errors.New("session frame is not newline terminated")
	ErrConnectionClosed = errors.New("session connection closed")
)

type Frame struct {
	JSONRPC               string          `json:"jsonrpc"`
	ID                    json.RawMessage `json:"id"`
	Method                string          `json:"method,omitempty"`
	Params                json.RawMessage `json:"params,omitempty"`
	Result                json.RawMessage `json:"result,omitempty"`
	Error                 *RPCError       `json:"error,omitempty"`
	errorNull, methodNull bool
}

func (f *Frame) UnmarshalJSON(body []byte) error {
	type wireFrame Frame
	if err := DecodeStrict(body, (*wireFrame)(f)); err != nil {
		return err
	}
	f.errorNull = explicitNull(body, "error")
	f.methodNull = explicitNull(body, "method")
	return nil
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "session RPC error"
	}
	return e.Message
}

type Connection struct {
	connection net.Conn
	reader     *bufio.Reader
	writeMu    sync.Mutex
	stateMu    sync.Mutex
	closed     bool
	closeDone  chan struct{}
	closeErr   error
	observeMu  sync.Mutex
	observer   func(string, Frame)
}

func NewConnection(connection net.Conn) *Connection {
	return &Connection{connection: connection, reader: bufio.NewReaderSize(connection, MaxFrameBytes+1), closeDone: make(chan struct{})}
}

func (c *Connection) Observe(observer func(string, Frame)) {
	c.observeMu.Lock()
	c.observer = observer
	c.observeMu.Unlock()
}

func (c *Connection) Write(frame Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return ErrConnectionClosed
	}
	c.stateMu.Unlock()
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	c.observe("send", frame)
	_, err = io.Copy(c.connection, bytes.NewReader(body))
	return err
}

func (c *Connection) observe(direction string, frame Frame) {
	c.observeMu.Lock()
	defer c.observeMu.Unlock()
	if c.observer != nil {
		c.observer(direction, frame)
	}
}

func (c *Connection) Close() error {
	c.stateMu.Lock()
	if c.closed {
		done := c.closeDone
		c.stateMu.Unlock()
		<-done
		return c.closeErr
	}
	c.closed = true
	c.stateMu.Unlock()
	err := c.connection.Close()
	c.stateMu.Lock()
	c.closeErr = err
	close(c.closeDone)
	c.stateMu.Unlock()
	return err
}

func ValidRequest(frame Frame) bool {
	return frame.JSONRPC == "2.0" && frame.Method != "" && len(frame.Params) != 0 &&
		json.Valid(frame.Params) && frame.Result == nil && frame.Error == nil && !frame.errorNull
}

func DecodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON value has trailing content")
	}
	return nil
}

func Success(id, result json.RawMessage) Frame {
	if result == nil {
		result = json.RawMessage(`null`)
	}
	return Frame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Result: result}
}

func Failure(id json.RawMessage, code int, message string, data any) Frame {
	var encoded json.RawMessage
	if data != nil {
		encoded, _ = json.Marshal(data)
	}
	return Frame{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Error: &RPCError{Code: code, Message: message, Data: encoded}}
}

func explicitNull(body []byte, field string) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(body, &object) == nil && bytes.Equal(bytes.TrimSpace(object[field]), []byte("null"))
}
