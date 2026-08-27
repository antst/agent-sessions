package daemon

import (
	"io"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

// hostDiagnosticWriter is the one process-local serialization boundary for
// metadata-only daemon output. The platform service manager owns its stdout
// and stderr destinations; the daemon never opens a parallel log authority.
type hostDiagnosticWriter struct {
	mu     sync.Mutex
	stdout io.Writer
	stderr io.Writer
}

func newHostDiagnosticWriter(stdout, stderr io.Writer) *hostDiagnosticWriter {
	return &hostDiagnosticWriter{stdout: stdout, stderr: stderr}
}

func (writer *hostDiagnosticWriter) emit(kind diagnostics.OutputKind, event string, fields map[string]any) {
	if writer == nil {
		return
	}
	body, err := diagnostics.Render(kind, event, fields)
	if err != nil {
		return
	}
	target := writer.stdout
	if kind == diagnostics.OutputError || kind == diagnostics.OutputCrashReport {
		target = writer.stderr
	}
	if target == nil {
		return
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	_, _ = target.Write(append(body, '\n'))
}

type controlObservation struct {
	RequestID string
	Operation string
	Role      controlRole
	Accepted  bool
	ErrorCode string
	Retryable bool
	Duration  time.Duration
}

func (writer *hostDiagnosticWriter) observeControl(observation controlObservation) {
	kind := diagnostics.OutputNormal
	state := "accepted"
	if !observation.Accepted {
		kind = diagnostics.OutputError
		state = "rejected"
	}
	writer.emit(kind, "host.control", map[string]any{
		"request_id":  observation.RequestID,
		"operation":   observation.Operation,
		"role":        string(observation.Role),
		"state":       state,
		"duration_ms": observation.Duration.Milliseconds(),
		"error_code":  observation.ErrorCode,
		"retryable":   observation.Retryable,
	})
}
