package structuredprocess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestFramerReadsBoundedFramesAndRecoversAfterOversize(t *testing.T) {
	reader, input := io.Pipe()
	output := &bufferWriteCloser{}
	framer, err := NewFramer(reader, output, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = framer.Close() })

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(input, "12345678\ntoo-large\nok\n")
		_ = input.Close()
		writeDone <- writeErr
	}()

	frame, err := framer.ReadFrame(context.Background())
	if err != nil || string(frame) != "12345678" {
		t.Fatalf("first frame = %q, %v; want boundary-sized frame", frame, err)
	}
	if _, err := framer.ReadFrame(context.Background()); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame error = %v; want ErrFrameTooLarge", err)
	}
	frame, err = framer.ReadFrame(context.Background())
	if err != nil || string(frame) != "ok" {
		t.Fatalf("frame after oversized input = %q, %v; want ok", frame, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestFramerRejectsUnterminatedAndInvalidOutboundFrames(t *testing.T) {
	output := &bufferWriteCloser{}
	framer, err := NewFramer(io.NopCloser(bytes.NewBufferString("unterminated")), output, 12)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = framer.Close() })

	if _, err := framer.ReadFrame(context.Background()); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("unterminated frame error = %v; want ErrMalformedFrame", err)
	}
	for name, frame := range map[string][]byte{
		"empty":    {},
		"newline":  []byte("one\ntwo"),
		"oversize": []byte("1234567890123"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := framer.WriteFrame(context.Background(), frame); err == nil {
				t.Fatal("invalid outbound frame was accepted")
			}
		})
	}
	if output.Len() != 0 {
		t.Fatalf("invalid frames wrote %q", output.Bytes())
	}
}

func TestFramerSerializesWholeWritesInAdmissionOrder(t *testing.T) {
	writer := newGateWriteCloser()
	framer, err := NewFramer(io.NopCloser(bytes.NewReader(nil)), writer, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = framer.Close() })

	firstDone := make(chan error, 1)
	go func() { firstDone <- framer.WriteFrame(context.Background(), []byte("first")) }()
	<-writer.entered
	secondDone := make(chan error, 1)
	go func() { secondDone <- framer.WriteFrame(context.Background(), []byte("second")) }()
	close(writer.release)

	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if writer.concurrent {
		t.Fatal("frame writes overlapped")
	}
	if got := writer.String(); got != "first\nsecond\n" {
		t.Fatalf("ordered output = %q", got)
	}
}

func TestFramerCancellationUnblocksReadAndWrite(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		reader, input := io.Pipe()
		framer, err := NewFramer(reader, &bufferWriteCloser{}, 32)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = input.Close()
			_ = framer.Close()
		})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if _, err := framer.ReadFrame(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("canceled read = %v; want deadline exceeded", err)
		}
	})

	t.Run("write", func(t *testing.T) {
		writer := newBlockingWriteCloser()
		framer, err := NewFramer(io.NopCloser(bytes.NewReader(nil)), writer, 32)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = framer.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if err := framer.WriteFrame(ctx, []byte("blocked")); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("canceled write = %v; want deadline exceeded", err)
		}
	})
}

func TestFramerRejectsUnboundedConfiguration(t *testing.T) {
	for _, limit := range []int{-1, MaximumFrameBytes + 1} {
		if _, err := NewFramer(io.NopCloser(bytes.NewReader(nil)), &bufferWriteCloser{}, limit); err == nil {
			t.Fatalf("limit %d was accepted", limit)
		}
	}
}

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

type gateWriteCloser struct {
	mu         sync.Mutex
	buffer     bytes.Buffer
	active     int
	concurrent bool
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func newGateWriteCloser() *gateWriteCloser {
	return &gateWriteCloser{entered: make(chan struct{}), release: make(chan struct{})}
}

func (writer *gateWriteCloser) Write(body []byte) (int, error) {
	writer.mu.Lock()
	writer.active++
	if writer.active > 1 {
		writer.concurrent = true
	}
	writer.mu.Unlock()
	writer.once.Do(func() {
		close(writer.entered)
		<-writer.release
	})
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.active--
	return writer.buffer.Write(body)
}

func (writer *gateWriteCloser) Close() error { return nil }
func (writer *gateWriteCloser) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.buffer.String()
}

type blockingWriteCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{closed: make(chan struct{})}
}

func (writer *blockingWriteCloser) Write([]byte) (int, error) {
	<-writer.closed
	return 0, io.ErrClosedPipe
}

func (writer *blockingWriteCloser) Close() error {
	writer.once.Do(func() { close(writer.closed) })
	return nil
}
