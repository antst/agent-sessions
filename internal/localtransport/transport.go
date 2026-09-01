package localtransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/socketpath"
)

const (
	defaultMaxFrameBytes  = 1 << 20
	defaultMaxNesting     = 32
	defaultMaxStringBytes = 256 << 10
)

var (
	// ErrInvalidLimits means a caller attempted to disable a transport bound.
	ErrInvalidLimits = errors.New("local transport limits are invalid")
	// ErrFrameSize means the length prefix or encoded body exceeded its bound.
	ErrFrameSize = errors.New("local transport frame size is invalid")
	// ErrFrameJSON means a frame was not one bounded UTF-8 JSON object.
	ErrFrameJSON = errors.New("local transport frame is not bounded JSON object")
)

// Limits fixes allocation and JSON-complexity bounds before a peer is read.
// Zero values fail closed; use DefaultLimits for production defaults.
type Limits struct {
	MaxFrameBytes  uint32
	MaxNesting     int
	MaxStringBytes int
}

// DefaultLimits returns the component transport's fixed production bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxFrameBytes:  defaultMaxFrameBytes,
		MaxNesting:     defaultMaxNesting,
		MaxStringBytes: defaultMaxStringBytes,
	}
}

func (l Limits) validate() error {
	if l.MaxFrameBytes == 0 || l.MaxNesting <= 0 || l.MaxStringBytes <= 0 || uint64(l.MaxStringBytes) > uint64(l.MaxFrameBytes) {
		return ErrInvalidLimits
	}
	return nil
}

// Valid reports whether every transport bound is explicitly enabled and
// internally consistent.
func (l Limits) Valid() bool { return l.validate() == nil }

// WriteFrame writes one validated four-byte big-endian length-prefixed JSON
// object. Validation happens before any bytes reach the writer.
func WriteFrame(writer io.Writer, body []byte, limits Limits) error {
	if err := limits.validate(); err != nil {
		return err
	}
	if len(body) == 0 || uint64(len(body)) > uint64(limits.MaxFrameBytes) {
		return ErrFrameSize
	}
	if err := validateJSON(body, limits); err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body))) //nolint:gosec // length is bounded above by uint32.
	if _, err := io.CopyN(writer, bytes.NewReader(header[:]), int64(len(header))); err != nil {
		return fmt.Errorf("write local frame header: %w", err)
	}
	if _, err := io.CopyN(writer, bytes.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("write local frame body: %w", err)
	}
	return nil
}

// ReadFrame checks the length before allocating and returns one validated
// UTF-8 JSON object.
func ReadFrame(reader io.Reader, limits Limits) ([]byte, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, fmt.Errorf("read local frame header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > limits.MaxFrameBytes {
		return nil, ErrFrameSize
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read local frame body: %w", err)
	}
	if err := validateJSON(body, limits); err != nil {
		return nil, err
	}
	return body, nil
}

func validateJSON(body []byte, limits Limits) error {
	if !utf8.Valid(body) {
		return fmt.Errorf("%w: invalid UTF-8", ErrFrameJSON)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFrameJSON, err)
	}
	if delimiter, ok := first.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("%w: root must be an object", ErrFrameJSON)
	}
	depth := 1
	for depth > 0 {
		token, tokenErr := decoder.Token()
		if tokenErr != nil {
			return fmt.Errorf("%w: %v", ErrFrameJSON, tokenErr)
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{', '[':
				depth++
				if depth > limits.MaxNesting {
					return fmt.Errorf("%w: nesting exceeds %d", ErrFrameJSON, limits.MaxNesting)
				}
			case '}', ']':
				depth--
			}
		case string:
			if len([]byte(value)) > limits.MaxStringBytes {
				return fmt.Errorf("%w: string exceeds %d bytes", ErrFrameJSON, limits.MaxStringBytes)
			}
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: trailing JSON value", ErrFrameJSON)
		}
		return fmt.Errorf("%w: trailing data: %v", ErrFrameJSON, err)
	}
	return nil
}

// Conn is one framed Unix stream. Reads and writes are independently safe for
// concurrent use; individual frames cannot interleave.
type Conn struct {
	connection *net.UnixConn
	limits     Limits
	readMu     sync.Mutex
	writeMu    sync.Mutex
}

func newConn(connection *net.UnixConn, limits Limits) *Conn {
	return &Conn{connection: connection, limits: limits}
}

// ReadFrame reads one bounded frame from the stream.
func (c *Conn) ReadFrame() ([]byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return ReadFrame(c.connection, c.limits)
}

// WriteFrame writes one bounded frame to the stream.
func (c *Conn) WriteFrame(body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteFrame(c.connection, body, c.limits)
}

// Close closes the stream.
func (c *Conn) Close() error { return c.connection.Close() }

// SetReadDeadline sets the underlying stream's read deadline.
func (c *Conn) SetReadDeadline(deadline time.Time) error {
	return c.connection.SetReadDeadline(deadline)
}

// SetWriteDeadline sets the underlying stream's write deadline.
func (c *Conn) SetWriteDeadline(deadline time.Time) error {
	return c.connection.SetWriteDeadline(deadline)
}

// Listener accepts framed connections and captures peer credentials before
// returning each stream to its caller.
type Listener struct {
	listener *net.UnixListener
	limits   Limits
}

// Listen binds a new socket below an existing private user-owned 0700
// directory. Existing paths and symlinks fail closed.
func Listen(path string, limits Limits) (*Listener, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	var err error
	path, err = noFollowSocketPath(path)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateParent(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("local socket path already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect local socket path: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on local socket: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("make local socket private: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || fileUID(info) != os.Geteuid() {
		_ = listener.Close()
		return nil, errors.New("local socket identity could not be corroborated")
	}
	return &Listener{listener: listener, limits: limits}, nil
}

// Accept captures the kernel peer PID and UID immediately after accept.
func (l *Listener) Accept() (*Conn, PeerIdentity, error) {
	connection, err := l.listener.AcceptUnix()
	if err != nil {
		return nil, PeerIdentity{}, err
	}
	peer, err := CapturePeerIdentity(connection)
	if err != nil {
		_ = connection.Close()
		return nil, PeerIdentity{}, fmt.Errorf("capture local peer identity: %w", err)
	}
	return newConn(connection, l.limits), peer, nil
}

// Close closes the listener and unlinks the socket it created.
func (l *Listener) Close() error { return l.listener.Close() }

// Dial connects only through an exact non-symlink socket below a private
// user-owned directory.
func Dial(path string, limits Limits) (*Conn, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	var err error
	path, err = noFollowSocketPath(path)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateParent(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect local socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || fileUID(info) != os.Geteuid() {
		return nil, errors.New("local socket is not an exact user-owned socket")
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	return newConn(connection, limits), nil
}

func validatePrivateParent(parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect local socket parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("local socket parent is not an exact directory")
	}
	if info.Mode().Perm() != 0o700 || fileUID(info) != os.Geteuid() {
		return errors.New("local socket parent must be user-owned mode 0700")
	}
	return nil
}

func noFollowSocketPath(path string) (string, error) {
	if err := socketpath.Validate(path); err != nil {
		return "", err
	}
	canonical, err := pathidentity.FuturePath(path)
	if err != nil {
		return "", fmt.Errorf("validate no-follow local socket path: %w", err)
	}
	if err := socketpath.Validate(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func fileUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}
