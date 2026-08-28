package federation

import (
	"errors"
	"io"
	"sync"
	"syscall"

	"github.com/antst/agent-sessions/internal/diagnostics"
)

// hubDiagnosticWriter is the one process-local serialization boundary for
// metadata-only hub output. The service manager owns the supplied streams;
// the hub never creates a parallel log or observability authority.
type hubDiagnosticWriter struct {
	mu     sync.Mutex
	stdout io.Writer
	stderr io.Writer
}

func newHubDiagnosticWriter(stdout, stderr io.Writer) *hubDiagnosticWriter {
	return &hubDiagnosticWriter{stdout: stdout, stderr: stderr}
}

func (writer *hubDiagnosticWriter) emit(kind diagnostics.OutputKind, event string, fields map[string]any) {
	if writer == nil {
		return
	}
	body, err := renderHubDiagnostic(kind, event, fields)
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

func hubResourceFailureFields(err error) map[string]any {
	fields := map[string]any{
		"operation": "host.register", "role": "hub", "state": "rejected",
		"retryable": true, "cause_detail": err,
	}
	switch {
	case errors.Is(err, syscall.ENOSPC):
		fields["error_code"] = "disk_full"
		fields["next_action"] = "free space in the hub state filesystem and retry"
	case errors.Is(err, syscall.ENOMEM):
		fields["error_code"] = "memory_unavailable"
		fields["next_action"] = "restore memory availability and retry"
	case errors.Is(err, syscall.EMFILE):
		fields["error_code"] = "file_descriptors_exhausted"
		fields["next_action"] = "restore the owning user's file descriptor availability and retry"
	case errors.Is(err, syscall.EAGAIN):
		fields["error_code"] = "process_resources_exhausted"
		fields["next_action"] = "restore process resources and retry"
	default:
		fields["error_code"] = "hub_admission_failed"
		fields["next_action"] = "inspect hub status and retry"
	}
	return fields
}
