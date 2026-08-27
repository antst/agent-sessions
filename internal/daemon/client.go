package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// LocalControlRole identifies one short-lived same-user daemon client boundary.
type LocalControlRole string

const (
	// LocalControlLauncher prepares or updates one managed native launch.
	LocalControlLauncher LocalControlRole = "launcher"
	// LocalControlConnector relays model-facing MCP operations for one attested attachment.
	LocalControlConnector LocalControlRole = "connector"
	// LocalControlHook reports vendor-native lifecycle evidence for one attested attachment.
	LocalControlHook LocalControlRole = "hook"
)

// LocalControlIdentity supplies the exact role and optional attachment attestation for one exchange.
type LocalControlIdentity struct {
	Role         LocalControlRole
	Product      string
	AttachmentID string
	SessionID    string
	Capability   string
	NativeActor  map[string]any
}

// LocalControlResult is one correlated daemon response.
type LocalControlResult struct {
	Generation       uint64
	ResourceRevision string
	Result           json.RawMessage
}

// LocalControlError preserves the daemon's stable machine-readable rejection.
type LocalControlError struct {
	Code      string
	Message   string
	Retryable bool
}

func (failure *LocalControlError) Error() string {
	return fmt.Sprintf("daemon rejected local control operation: %s", failure.Message)
}

// QueryLocalControl performs one bounded request against the existing daemon and never manages service lifetime.
func QueryLocalControl(ctx context.Context, identity LocalControlIdentity, operation string, payload any) (LocalControlResult, error) {
	paths, err := ResolveProductionPaths()
	if err != nil {
		return LocalControlResult{}, &UnavailableError{Cause: err, NextAction: daemonInspectionCommand()}
	}
	connection, err := DialControlEndpoint(ctx, paths.ControlEndpoint)
	if err != nil {
		return LocalControlResult{}, err
	}
	defer func() { _ = connection.Close() }()
	return queryLocalControlConnection(ctx, connection, identity, operation, payload)
}

// PrepareManagedAttachment durably reserves one launch and returns its non-durable raw capability.
func PrepareManagedAttachment(ctx context.Context, request AttachmentPrepareRequest) (AttachmentPrepareResult, error) {
	response, err := QueryLocalControl(ctx, LocalControlIdentity{Role: LocalControlLauncher}, "attachment.prepare", request)
	if err != nil {
		return AttachmentPrepareResult{}, err
	}
	var result AttachmentPrepareResult
	if err := decodeStrictControlFrame(response.Result, &result); err != nil {
		return AttachmentPrepareResult{}, fmt.Errorf("decode prepared attachment: %w", err)
	}
	if result.Attachment.AttachmentID == "" || result.Capability == "" {
		return AttachmentPrepareResult{}, errors.New("daemon returned an incomplete prepared attachment")
	}
	return result, nil
}

// DetachManagedAttachment rolls back or ends one exact attachment through the launcher role.
func DetachManagedAttachment(ctx context.Context, attachmentID, reason string) error {
	_, err := QueryLocalControl(ctx, LocalControlIdentity{Role: LocalControlLauncher}, "attachment.detach", AttachmentDetachRequest{
		AttachmentID: attachmentID, Reason: reason,
	})
	return err
}

//nolint:gocyclo // One bounded exchange validates the complete hello/request/response correlation contract.
func queryLocalControlConnection(
	ctx context.Context,
	connection net.Conn,
	identity LocalControlIdentity,
	operation string,
	payload any,
) (LocalControlResult, error) {
	if identity.Role != LocalControlLauncher && identity.Role != LocalControlConnector && identity.Role != LocalControlHook {
		return LocalControlResult{}, fmt.Errorf("unsupported local control role %q", identity.Role)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return LocalControlResult{}, err
	}
	helloID, err := randomControlRequestID()
	if err != nil {
		return LocalControlResult{}, err
	}
	if err := writeControlFrame(connection, controlHello{
		Type: "hello", Version: localControlProtocolVersion, RequestID: helloID, Role: controlRole(identity.Role),
		Product: identity.Product, AttachmentID: identity.AttachmentID, SessionID: identity.SessionID,
		Capability: identity.Capability, NativeActor: identity.NativeActor,
	}); err != nil {
		return LocalControlResult{}, err
	}
	reader := bufio.NewReaderSize(connection, 64*1024)
	body, err := readBoundedControlFrame(reader)
	if err != nil {
		return LocalControlResult{}, err
	}
	var hello struct {
		Type             string `json:"type"`
		Version          int    `json:"version"`
		RequestID        string `json:"request_id"`
		DaemonGeneration uint64 `json:"daemon_generation"`
	}
	if err := json.Unmarshal(body, &hello); err != nil || hello.Type != "hello.result" || hello.RequestID != helloID || hello.DaemonGeneration == 0 {
		return LocalControlResult{}, errors.New("daemon returned an invalid local control hello")
	}
	payloadBody, err := json.Marshal(payload)
	if err != nil {
		return LocalControlResult{}, fmt.Errorf("encode local control payload: %w", err)
	}
	requestID, err := randomControlRequestID()
	if err != nil {
		return LocalControlResult{}, err
	}
	if err := writeControlFrame(connection, controlRequest{
		Type: "request", Version: localControlProtocolVersion, RequestID: requestID,
		Operation: operation, ExpectedGeneration: hello.DaemonGeneration, Payload: payloadBody,
	}); err != nil {
		return LocalControlResult{}, err
	}
	body, err = readBoundedControlFrame(reader)
	if err != nil {
		return LocalControlResult{}, err
	}
	var response controlResponse
	if err := decodeStrictControlFrame(body, &response); err != nil || response.Type != "response" || response.RequestID != requestID || response.DaemonGeneration == 0 {
		return LocalControlResult{}, errors.New("daemon returned an invalid local control response")
	}
	if !response.Accepted {
		if response.Error == nil {
			return LocalControlResult{}, errors.New("daemon rejected local control without a cause")
		}
		return LocalControlResult{}, &LocalControlError{
			Code: response.Error.Code, Message: response.Error.Message, Retryable: response.Error.Retryable,
		}
	}
	return LocalControlResult{
		Generation: response.DaemonGeneration, ResourceRevision: response.ResourceRevision,
		Result: append(json.RawMessage(nil), response.Result...),
	}, nil
}

func randomControlRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate local control request id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
