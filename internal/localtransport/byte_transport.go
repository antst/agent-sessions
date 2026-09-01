package localtransport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ByteConn is a length-bounded binary Unix stream. Unlike Conn it deliberately
// performs no JSON decoding; protocol-specific validation belongs to its sole
// caller. Reads and writes remain independently serialized.
type ByteConn struct {
	connection *net.UnixConn
	maxBytes   uint32
	readMu     sync.Mutex
	writeMu    sync.Mutex
}

// FrameWriteOutcome reports how much of one length-prefixed frame reached the
// socket writer. Callers use it to distinguish a proven zero-byte failure from
// a partial/possible write and a complete frame.
type FrameWriteOutcome struct {
	WrittenBytes int
	TotalBytes   int
}

func (o FrameWriteOutcome) Complete() bool {
	return o.TotalBytes > 0 && o.WrittenBytes == o.TotalBytes
}

func (o FrameWriteOutcome) Zero() bool {
	return o.TotalBytes > 0 && o.WrittenBytes == 0
}

func newByteConn(connection *net.UnixConn, maxBytes uint32) (*ByteConn, error) {
	if connection == nil || maxBytes == 0 {
		return nil, ErrInvalidLimits
	}
	if err := closeOnExec(connection); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("set local binary transport close-on-exec: %w", err)
	}
	return &ByteConn{connection: connection, maxBytes: maxBytes}, nil
}

func (c *ByteConn) ReadFrame() ([]byte, error) {
	if c == nil || c.connection == nil || c.maxBytes == 0 {
		return nil, ErrInvalidLimits
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return readByteFrame(c.connection, c.maxBytes)
}

func readByteFrame(reader io.Reader, maxBytes uint32) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, fmt.Errorf("read local binary frame header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxBytes {
		return nil, ErrFrameSize
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(reader, body); err != nil {
		zeroByteFrame(body)
		return nil, fmt.Errorf("read local binary frame body: %w", err)
	}
	return body, nil
}

func (c *ByteConn) WriteFrame(body []byte) error {
	_, err := c.WriteFrameOutcome(body)
	return err
}

// WriteFrameOutcome writes one frame and preserves the exact byte count on
// error. A partial count is security-significant to one-shot commit protocols.
func (c *ByteConn) WriteFrameOutcome(body []byte) (FrameWriteOutcome, error) {
	if c == nil || c.connection == nil || c.maxBytes == 0 {
		return FrameWriteOutcome{}, ErrInvalidLimits
	}
	if len(body) == 0 || uint64(len(body)) > uint64(c.maxBytes) {
		return FrameWriteOutcome{}, ErrFrameSize
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeByteFrame(c.connection, body)
}

func writeByteFrame(writer io.Writer, body []byte) (FrameWriteOutcome, error) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body))) //nolint:gosec // bounded by maxBytes.
	frame := io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(body))
	total := len(header) + len(body)
	written, err := io.CopyN(writer, frame, int64(total))
	outcome := FrameWriteOutcome{WrittenBytes: int(written), TotalBytes: total}
	if err != nil {
		return outcome, fmt.Errorf("write local binary frame: %w", err)
	}
	if written != int64(total) {
		return outcome, io.ErrShortWrite
	}
	return outcome, nil
}

func (c *ByteConn) Close() error { return c.connection.Close() }
func (c *ByteConn) SetDeadline(deadline time.Time) error {
	return c.connection.SetDeadline(deadline)
}

// ByteListener shares the JSON transport's private, no-follow socket path and
// exact kernel peer-credential admission, while exposing only bounded bytes.
type ByteListener struct {
	listener *net.UnixListener
	maxBytes uint32
}

func ListenBytes(path string, maxBytes uint32) (*ByteListener, error) {
	if maxBytes == 0 {
		return nil, ErrInvalidLimits
	}
	listener, err := listenUnix(path)
	if err != nil {
		return nil, err
	}
	return &ByteListener{listener: listener, maxBytes: maxBytes}, nil
}

func (l *ByteListener) Accept() (*ByteConn, PeerIdentity, error) {
	if l == nil || l.listener == nil || l.maxBytes == 0 {
		return nil, PeerIdentity{}, ErrInvalidLimits
	}
	connection, err := l.listener.AcceptUnix()
	if err != nil {
		return nil, PeerIdentity{}, err
	}
	peer, err := CapturePeerIdentity(connection)
	if err != nil {
		_ = connection.Close()
		return nil, PeerIdentity{}, fmt.Errorf("capture local binary peer identity: %w", err)
	}
	bounded, err := newByteConn(connection, l.maxBytes)
	if err != nil {
		return nil, PeerIdentity{}, err
	}
	return bounded, peer, nil
}

func (l *ByteListener) Close() error {
	if l == nil || l.listener == nil {
		return errors.New("local binary listener is nil")
	}
	return l.listener.Close()
}

func DialBytes(path string, maxBytes uint32) (*ByteConn, error) {
	if maxBytes == 0 {
		return nil, ErrInvalidLimits
	}
	connection, err := dialUnix(path)
	if err != nil {
		return nil, err
	}
	return newByteConn(connection, maxBytes)
}

func zeroByteFrame(body []byte) {
	for index := range body {
		body[index] = 0
	}
}
