package bridge

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	grokDiagnosticTailLimit   = 64 * 1024
	grokDiagnosticFileLimit   = 2 * grokDiagnosticTailLimit
	grokDiagnosticJoinTimeout = 500 * time.Millisecond
)

// grokDiagnosticSink keeps background Grok output away from the interactive
// terminal. The private file appends cheaply up to a bounded high-water mark,
// then compacts to the newest logical tail. Raw bytes are never copied into
// control errors, wake records, or terminal output.
type grokDiagnosticSink struct {
	mu          sync.Mutex
	file        *os.File
	fileBytes   int
	tail        []byte
	writeErr    error
	closed      bool
	compactions int
}

type grokProcessDiagnostics struct {
	role string
	sink *grokDiagnosticSink
}

func newGrokDiagnosticSink(path string) (*grokDiagnosticSink, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600) // #nosec G304 -- The path is the bridge-owned per-launch diagnostic file.
	if err != nil {
		return nil, errors.New("create private Grok diagnostic log failed")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, errors.New("secure private Grok diagnostic log failed")
	}
	return &grokDiagnosticSink{file: file}, nil
}

func (s *grokDiagnosticSink) process(role string) *grokProcessDiagnostics {
	return &grokProcessDiagnostics{role: role, sink: s}
}

func (s *grokDiagnosticSink) Write(body []byte) (int, error) {
	written := len(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return written, nil
	}
	s.tail = appendBoundedBytes(s.tail, body, grokDiagnosticTailLimit)
	if s.file != nil && s.writeErr == nil {
		if s.fileBytes+len(body) > grokDiagnosticFileLimit {
			if _, err := s.file.Seek(0, 0); err != nil {
				s.writeErr = err
			} else if _, err := s.file.Write(s.tail); err != nil {
				s.writeErr = err
			} else if err := s.file.Truncate(int64(len(s.tail))); err != nil {
				s.writeErr = err
			} else {
				s.fileBytes = len(s.tail)
				s.compactions++
			}
		} else {
			if _, err := s.file.Seek(0, 2); err != nil {
				s.writeErr = err
			} else {
				n, err := s.file.Write(body)
				s.fileBytes += n
				if err != nil {
					s.writeErr = err
				}
			}
		}
	}
	// A diagnostic-file failure must never turn a healthy managed process into
	// a broken-pipe failure. Diagnostics remain best-effort and private.
	return written, nil
}

func (s *grokDiagnosticSink) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.writeErr
	}
	s.closed = true
	if s.file != nil {
		if err := s.file.Close(); err != nil && s.writeErr == nil {
			s.writeErr = err
		}
		s.file = nil
	}
	return s.writeErr
}

func (d *grokProcessDiagnostics) Write(body []byte) (int, error) {
	written := len(body)
	if d != nil && d.sink != nil {
		_, _ = d.sink.Write(append([]byte("["+d.role+"] "), body...))
	}
	return written, nil
}

func (d *grokProcessDiagnostics) recordFailure(cause error) {
	if d == nil || d.sink == nil || cause == nil {
		return
	}
	detail := cause.Error()
	if len(detail) > grokDiagnosticTailLimit {
		detail = detail[len(detail)-grokDiagnosticTailLimit:]
	}
	_, _ = d.Write([]byte("managed process failure: " + detail + "\n"))
}

func (d *grokProcessDiagnostics) safeError(role string, joined bool) error {
	if !joined {
		return fmt.Errorf("%s; managed process join incomplete; details captured in private host diagnostics", role)
	}
	return fmt.Errorf("%s; details captured in private host diagnostics", role)
}

func appendBoundedBytes(current, added []byte, limit int) []byte {
	if len(added) >= limit {
		return append(current[:0], added[len(added)-limit:]...)
	}
	if excess := len(current) + len(added) - limit; excess > 0 {
		copy(current, current[excess:])
		current = current[:len(current)-excess]
	}
	return append(current, added...)
}
