package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// QueryAdmin performs one correlated read-only administrative request against
// the already-running production daemon. It never starts service lifetime.
func QueryAdmin(ctx context.Context, operation string) (json.RawMessage, error) {
	if operation != "runtime.status" && operation != "runtime.doctor" && operation != "remove.inspect" {
		return nil, fmt.Errorf("unsupported online administrative operation %q", operation)
	}
	paths, err := ResolveProductionPaths()
	if err != nil {
		return nil, &UnavailableError{Cause: err, NextAction: daemonInspectionCommand()}
	}
	connection, err := DialControlEndpoint(ctx, paths.ControlEndpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	return queryAdminConnection(ctx, connection, operation)
}

//nolint:gocyclo // One bounded exchange validates every hello and correlated response invariant together.
func queryAdminConnection(ctx context.Context, connection net.Conn, operation string) (json.RawMessage, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, err
	}
	helloID := "admin-hello"
	if err := writeControlFrame(connection, controlHello{
		Type: "hello", Version: localControlProtocolVersion, RequestID: helloID, Role: controlRoleAdmin,
	}); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(connection, 64*1024)
	body, err := readBoundedControlFrame(reader)
	if err != nil {
		return nil, err
	}
	var hello struct {
		Type             string `json:"type"`
		Version          int    `json:"version"`
		RequestID        string `json:"request_id"`
		DaemonGeneration uint64 `json:"daemon_generation"`
		RuntimeVersion   string `json:"runtime_version"`
		Role             string `json:"role"`
	}
	if err := decodeStrictControlFrame(body, &hello); err != nil || hello.Type != "hello.result" || hello.RequestID != helloID || hello.DaemonGeneration == 0 {
		return nil, errors.New("daemon returned an invalid administrative hello")
	}
	requestID := "admin-request"
	if err := writeControlFrame(connection, controlRequest{
		Type: "request", Version: localControlProtocolVersion, RequestID: requestID,
		Operation: operation, ExpectedGeneration: hello.DaemonGeneration, Payload: json.RawMessage(`{}`),
	}); err != nil {
		return nil, err
	}
	body, err = readBoundedControlFrame(reader)
	if err != nil {
		return nil, err
	}
	var response controlResponse
	if err := decodeStrictControlFrame(body, &response); err != nil || response.Type != "response" || response.RequestID != requestID {
		return nil, errors.New("daemon returned an invalid administrative response")
	}
	if !response.Accepted {
		if response.Error == nil {
			return nil, errors.New("daemon rejected administration without a cause")
		}
		return nil, &AdministrativeError{
			Operation: operation, Code: response.Error.Code, Message: response.Error.Message,
			Retryable: response.Error.Retryable, NextAction: response.Error.NextAction,
		}
	}
	return append(json.RawMessage(nil), response.Result...), nil
}

// AdministrativeError preserves one daemon-classified, metadata-only admin
// refusal through the local client and stable public CLI envelope.
type AdministrativeError struct {
	Operation  string
	Code       string
	Message    string
	Retryable  bool
	NextAction string
}

func (failure *AdministrativeError) Error() string {
	if failure == nil {
		return "administrative operation failed"
	}
	return fmt.Sprintf("daemon rejected %s: %s", failure.Operation, failure.Message)
}

// ExitCode maps daemon metadata to the stable public semantic classes.
func (failure *AdministrativeError) ExitCode() int {
	if failure == nil {
		return 1
	}
	if failure.Code == "operation_unavailable" {
		return 3
	}
	if failure.Retryable {
		return 6
	}
	return 1
}

func runtimeAdminDispatch(runtime *Runtime) func(context.Context, controlPrincipal, controlRequest) (controlDispatchResult, *controlError) {
	return func(_ context.Context, _ controlPrincipal, request controlRequest) (controlDispatchResult, *controlError) {
		var result any
		switch request.Operation {
		case "runtime.status":
			result = runtime.StatusProjection()
		case "runtime.doctor":
			result = runtime.DoctorProjection()
		case "remove.inspect":
			result = runtime.RemovalInspection()
		default:
			return controlDispatchResult{}, &controlError{Code: "operation_unavailable", Message: "administrative operation is not implemented", Retryable: false}
		}
		body, err := json.Marshal(result)
		if err != nil {
			return controlDispatchResult{}, &controlError{Code: "internal", Message: "encode administrative result", Retryable: true}
		}
		return controlDispatchResult{Result: body}, nil
	}
}
