//go:build linux || darwin

package localtransport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestByteTransportRoundTripBoundsAndPeerIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "launch.sock")
	listener, err := ListenBytes(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan PeerIdentity, 1)
	errs := make(chan error, 1)
	go func() {
		connection, peer, acceptErr := listener.Accept()
		if acceptErr != nil {
			errs <- acceptErr
			return
		}
		defer connection.Close()
		body, readErr := connection.ReadFrame()
		if readErr == nil && !bytes.Equal(body, []byte{0, 1, 2, 0xff}) {
			readErr = errors.New("binary body changed")
		}
		if readErr == nil {
			readErr = connection.WriteFrame([]byte{0xfe, 0, 3})
		}
		if readErr != nil {
			errs <- readErr
			return
		}
		accepted <- peer
	}()
	client, err := DialBytes(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	assertByteConnCloseOnExec(t, client)
	if err := client.WriteFrame([]byte{0, 1, 2, 0xff}); err != nil {
		t.Fatal(err)
	}
	body, err := client.ReadFrame()
	if err != nil || !bytes.Equal(body, []byte{0xfe, 0, 3}) {
		t.Fatalf("binary response = %v, %v", body, err)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case peer := <-accepted:
		if peer.PID != os.Getpid() || peer.UID != os.Geteuid() {
			t.Fatalf("peer = %+v", peer)
		}
	}
}

func assertByteConnCloseOnExec(t *testing.T, connection *ByteConn) {
	t.Helper()
	raw, err := connection.connection.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		flags, controlErr = unix.FcntlInt(fd, unix.F_GETFD, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if controlErr != nil {
		t.Fatal(controlErr)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("binary local socket is not close-on-exec")
	}
}

func TestByteTransportRejectsOversizedLengthBeforeAllocation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "launch.sock")
	listener, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	errs := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			errs <- acceptErr
			return
		}
		defer connection.Close()
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], 33)
		_, writeErr := connection.Write(header[:])
		errs <- writeErr
	}()
	client, err := DialBytes(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ReadFrame(); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("oversized error = %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestByteFrameWriteOutcomeDistinguishesZeroPartialAndFull(t *testing.T) {
	body := []byte("1234567")
	for _, test := range []struct {
		name    string
		writer  io.Writer
		written int
		full    bool
	}{
		{name: "zero", writer: &limitedFailureWriter{limit: 0}, written: 0},
		{name: "partial", writer: &limitedFailureWriter{limit: 5}, written: 5},
		{name: "full", writer: &bytes.Buffer{}, written: 11, full: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := writeByteFrame(test.writer, body)
			if outcome.WrittenBytes != test.written || outcome.TotalBytes != 11 || outcome.Complete() != test.full || outcome.Zero() != (test.written == 0) {
				t.Fatalf("outcome = %+v, full=%t", outcome, outcome.Complete())
			}
			if test.full && err != nil {
				t.Fatal(err)
			}
			if !test.full && err == nil {
				t.Fatal("failed write returned nil error")
			}
		})
	}
}

func TestPartialReadBodyIsZeroedBeforeError(t *testing.T) {
	reader := &retainingPartialReader{}
	if _, err := readByteFrame(reader, 32); err == nil {
		t.Fatal("partial body read unexpectedly succeeded")
	}
	if len(reader.body) != 8 {
		t.Fatalf("retained body length = %d", len(reader.body))
	}
	for index, value := range reader.body {
		if value != 0 {
			t.Fatalf("partial body byte %d retained %#x", index, value)
		}
	}
}

type limitedFailureWriter struct {
	limit   int
	written int
}

func (w *limitedFailureWriter) Write(body []byte) (int, error) {
	if w.written >= w.limit {
		return 0, io.ErrClosedPipe
	}
	allowed := w.limit - w.written
	if allowed > len(body) {
		allowed = len(body)
	}
	w.written += allowed
	if allowed < len(body) || w.written == w.limit {
		return allowed, io.ErrClosedPipe
	}
	return allowed, nil
}

type retainingPartialReader struct {
	call int
	body []byte
}

func (r *retainingPartialReader) Read(destination []byte) (int, error) {
	r.call++
	if r.call == 1 {
		binary.BigEndian.PutUint32(destination, 8)
		return 4, nil
	}
	r.body = destination
	copy(destination, []byte("secret"))
	return len("secret"), io.ErrUnexpectedEOF
}
