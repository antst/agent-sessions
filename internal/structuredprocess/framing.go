package structuredprocess

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

const (
	// DefaultMaxFrameBytes bounds one structured protocol frame when callers do
	// not select a narrower product-specific limit.
	DefaultMaxFrameBytes = 1 << 20
	// MaximumFrameBytes prevents configuration from turning framed I/O into an
	// unbounded allocation surface.
	MaximumFrameBytes = 16 << 20
	maximumReadBuffer = 64 << 10
)

var (
	ErrFrameTooLarge  = errors.New("structured process frame exceeds limit")
	ErrMalformedFrame = errors.New("structured process frame is malformed")
	ErrFramerClosed   = errors.New("structured process framer is closed")
)

// Framer transports newline-delimited protocol frames. It owns the supplied
// pipe ends so a canceled blocking operation can be interrupted without a
// leaked goroutine. Cancellation is therefore terminal for that pipe
// direction, while an oversized input frame is drained and the reader remains
// synchronized at the next frame boundary.
type Framer struct {
	reader io.ReadCloser
	input  *bufio.Reader
	writer io.WriteCloser
	limit  int

	readGate  chan struct{}
	writeGate chan struct{}
	readOnce  sync.Once
	writeOnce sync.Once
	closeMu   sync.Mutex
	closed    bool
}

// NewFramer takes ownership of reader and writer. A zero limit selects
// DefaultMaxFrameBytes; limits above MaximumFrameBytes are rejected.
func NewFramer(reader io.ReadCloser, writer io.WriteCloser, limit int) (*Framer, error) {
	if reader == nil || writer == nil {
		return nil, errors.New("structured process framing requires reader and writer")
	}
	if limit == 0 {
		limit = DefaultMaxFrameBytes
	}
	if limit < 1 || limit > MaximumFrameBytes {
		return nil, errors.New("structured process frame limit is outside the bounded range")
	}
	bufferSize := limit + 1
	if bufferSize > maximumReadBuffer {
		bufferSize = maximumReadBuffer
	}
	framer := &Framer{
		reader:    reader,
		input:     bufio.NewReaderSize(reader, bufferSize),
		writer:    writer,
		limit:     limit,
		readGate:  make(chan struct{}, 1),
		writeGate: make(chan struct{}, 1),
	}
	framer.readGate <- struct{}{}
	framer.writeGate <- struct{}{}
	return framer, nil
}

// ReadFrame reads one complete frame without its newline delimiter. Concurrent
// readers are serialized so input order is never consumed out of sequence.
func (framer *Framer) ReadFrame(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("structured process read requires context")
	}
	if err := framer.acquire(ctx, framer.readGate); err != nil {
		return nil, err
	}
	defer framer.release(framer.readGate)
	if framer.isClosed() {
		return nil, ErrFramerClosed
	}

	stop := context.AfterFunc(ctx, func() { framer.closeReader() })
	frame, err := framer.readBoundedFrame()
	if !stop() && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return frame, err
}

// WriteFrame writes exactly one newline-delimited frame. Whole writes are
// serialized, so concurrent callers cannot interleave payload bytes.
func (framer *Framer) WriteFrame(ctx context.Context, frame []byte) error {
	if ctx == nil {
		return errors.New("structured process write requires context")
	}
	if len(frame) == 0 || bytes.IndexByte(frame, '\n') >= 0 {
		return ErrMalformedFrame
	}
	if len(frame) > framer.limit {
		return ErrFrameTooLarge
	}
	if err := framer.acquire(ctx, framer.writeGate); err != nil {
		return err
	}
	defer framer.release(framer.writeGate)
	if framer.isClosed() {
		return ErrFramerClosed
	}

	body := make([]byte, len(frame)+1)
	copy(body, frame)
	body[len(frame)] = '\n'
	stop := context.AfterFunc(ctx, func() { framer.closeWriter() })
	err := writeAll(framer.writer, body)
	if !stop() && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Close interrupts active I/O and rejects future operations.
func (framer *Framer) Close() error {
	framer.closeMu.Lock()
	framer.closed = true
	framer.closeMu.Unlock()
	readerErr := framer.closeReader()
	writerErr := framer.closeWriter()
	return errors.Join(readerErr, writerErr)
}

func (framer *Framer) readBoundedFrame() ([]byte, error) {
	frame := make([]byte, 0, min(framer.limit, maximumReadBuffer))
	oversized := false
	for {
		fragment, err := framer.input.ReadSlice('\n')
		complete := err == nil
		if complete {
			fragment = fragment[:len(fragment)-1]
		}
		if !oversized {
			if len(fragment) > framer.limit-len(frame) {
				oversized = true
				frame = nil
			} else {
				frame = append(frame, fragment...)
			}
		}

		switch {
		case complete && oversized:
			return nil, ErrFrameTooLarge
		case complete && len(frame) == 0:
			return nil, ErrMalformedFrame
		case complete:
			return frame, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && oversized:
			return nil, ErrFrameTooLarge
		case errors.Is(err, io.EOF) && len(frame) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, ErrMalformedFrame
		default:
			return nil, err
		}
	}
}

func (framer *Framer) acquire(ctx context.Context, gate <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate:
		return nil
	}
}

func (*Framer) release(gate chan<- struct{}) { gate <- struct{}{} }

func (framer *Framer) isClosed() bool {
	framer.closeMu.Lock()
	defer framer.closeMu.Unlock()
	return framer.closed
}

func (framer *Framer) closeReader() error {
	var err error
	framer.readOnce.Do(func() { err = framer.reader.Close() })
	return err
}

func (framer *Framer) closeWriter() error {
	var err error
	framer.writeOnce.Do(func() { err = framer.writer.Close() })
	return err
}

func writeAll(writer io.Writer, body []byte) error {
	for len(body) > 0 {
		written, err := writer.Write(body)
		if err != nil {
			return err
		}
		if written < 1 || written > len(body) {
			return io.ErrNoProgress
		}
		body = body[written:]
	}
	return nil
}
