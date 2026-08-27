package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
)

// ErrDaemonUnavailable classifies a control connection that could not reach
// the one user-managed host daemon. Callers must report this condition; they
// must never use it as a reason to bootstrap or replace the service.
var ErrDaemonUnavailable = errors.New("agent-sessions daemon is unavailable")

// UnavailableError preserves the failed endpoint and the platform's
// standard inspection command without exposing an implicit recovery action.
type UnavailableError struct {
	Endpoint   string
	Cause      error
	NextAction string
}

func (failure *UnavailableError) Error() string {
	if failure == nil {
		return ErrDaemonUnavailable.Error()
	}
	message := ErrDaemonUnavailable.Error()
	if failure.Endpoint != "" {
		message += fmt.Sprintf(" at %s", failure.Endpoint)
	}
	if failure.NextAction != "" {
		message += "; inspect with " + failure.NextAction
	}
	return message
}

func (failure *UnavailableError) Unwrap() error {
	if failure == nil || failure.Cause == nil {
		return ErrDaemonUnavailable
	}
	return errors.Join(ErrDaemonUnavailable, failure.Cause)
}

// ExitCode is the stable unavailable class from the CLI contract.
func (*UnavailableError) ExitCode() int { return 3 }

func daemonInspectionCommand() string {
	if runtime.GOOS == "darwin" {
		return "launchctl print gui/$UID/net.antst.agent-sessions"
	}
	return "systemctl --user status agent-sessions.service"
}

// InspectionCommand returns the platform's read-only standard service
// inspection action. It never starts or repairs the service.
func InspectionCommand() string { return daemonInspectionCommand() }

// DialControlEndpoint connects to the existing user daemon only. It contains
// deliberately no service-manager or process-starting fallback.
func DialControlEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, &UnavailableError{
			Endpoint: endpoint, Cause: err, NextAction: daemonInspectionCommand(),
		}
	}
	return connection, nil
}

type controlError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
	NextAction string `json:"next_action,omitempty"`
}

type controlDispatchResult struct {
	ResourceRevision string
	Result           json.RawMessage
}

func rejectedControlResponse(request controlRequest, generation uint64, failure *controlError) controlResponse {
	if failure == nil {
		failure = &controlError{Code: "internal", Message: "control operation failed", Retryable: false}
	}
	return controlResponse{
		Type: "response", Version: localControlProtocolVersion, RequestID: request.RequestID,
		Operation: request.Operation, DaemonGeneration: generation, Accepted: false, Error: failure,
	}
}
