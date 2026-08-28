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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

const (
	// InternalAttachmentIDEnvironment is inherited only by a daemon-prepared vendor process tree.
	InternalAttachmentIDEnvironment = "AGENT_SESSIONS_INTERNAL_ATTACHMENT_ID"
	// InternalCapabilityEnvironment carries one daemon-issued non-durable launch capability.
	InternalCapabilityEnvironment = "AGENT_SESSIONS_INTERNAL_LAUNCH_CAPABILITY"
	// InternalProductEnvironment identifies the prepared vendor product.
	InternalProductEnvironment = "AGENT_SESSIONS_INTERNAL_PRODUCT"
	// InternalSessionIDEnvironment carries a daemon-selected native session when known before exec.
	InternalSessionIDEnvironment = "AGENT_SESSIONS_INTERNAL_SESSION_ID"
	// HostBinaryEnvironment pins connector subprocesses to the same installed host image as their launcher.
	HostBinaryEnvironment = "AGENT_SESSIONS_HOST_BINARY"
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

// LookupManagedAttachment returns one daemon-owned durable session projection
// for exact generic resume or product-scoped name selection.
func LookupManagedAttachment(ctx context.Context, request AttachmentLookupRequest) (ManagedAttachment, error) {
	response, err := QueryLocalControl(ctx, LocalControlIdentity{Role: LocalControlLauncher}, "attachment.lookup", request)
	if err != nil {
		return ManagedAttachment{}, err
	}
	var result ManagedAttachment
	if err := decodeStrictControlFrame(response.Result, &result); err != nil {
		return ManagedAttachment{}, fmt.Errorf("decode managed attachment: %w", err)
	}
	if result.SessionID == "" || result.Product == "" || result.Kind == "" {
		return ManagedAttachment{}, errors.New("daemon returned an incomplete managed attachment")
	}
	return result, nil
}

// ResolveManagedParentContext returns the current connector's daemon-attested
// attachment identity. The caller cannot inspect or borrow another session.
func ResolveManagedParentContext(ctx context.Context, identity LocalControlIdentity, sessionID string) (federation.ParentContext, error) {
	response, err := QueryLocalControl(ctx, identity, "attachment.context", AttachmentLookupRequest{SessionID: sessionID})
	if err != nil {
		return federation.ParentContext{}, err
	}
	var result federation.ParentContext
	if err := decodeStrictControlFrame(response.Result, &result); err != nil {
		return federation.ParentContext{}, fmt.Errorf("decode managed parent context: %w", err)
	}
	if result.InstanceID == "" || result.SessionID == "" || result.Product == "" {
		return federation.ParentContext{}, errors.New("daemon returned an incomplete parent context")
	}
	return result, nil
}

// RouteManagedAgentFrame submits one product-neutral delivery through the
// daemon's attachment and delivery authorities.
func RouteManagedAgentFrame(ctx context.Context, identity LocalControlIdentity, frame federation.AgentFrame) (federation.AgentFrameResult, error) {
	result := federation.AgentFrameResult{Version: federation.AgentFrameVersion, MessageID: frame.MessageID}
	switch frame.Type {
	case "discover":
		response, err := QueryLocalControl(ctx, identity, "peer.discover", struct{}{})
		if err != nil {
			return federation.AgentFrameResult{}, err
		}
		if err := decodeStrictControlFrame(response.Result, &result.Peers); err != nil {
			return federation.AgentFrameResult{}, fmt.Errorf("decode peer discovery: %w", err)
		}
		result.Type = "discover.result"
		return result, nil
	case "send", "broadcast":
		operation := "peer.send"
		payload := DeliveryRequest{
			MessageID: frame.MessageID, Targets: append([]string(nil), frame.Targets...), Group: frame.Group,
			Content: frame.Content, Summary: frame.Summary, SentAt: frame.SentAt,
		}
		if frame.Type == "broadcast" {
			operation = "peer.broadcast"
		}
		response, err := QueryLocalControl(ctx, identity, operation, payload)
		if err != nil {
			return federation.AgentFrameResult{}, err
		}
		var delivery DeliveryRecord
		if err := decodeStrictControlFrame(response.Result, &delivery); err != nil {
			return federation.AgentFrameResult{}, fmt.Errorf("decode peer delivery: %w", err)
		}
		result.Type = frame.Type + ".result"
		for target, status := range delivery.DestinationResults {
			if status == DeliveryDestinationDelivered {
				status = "accepted"
			}
			result.Deliveries = append(result.Deliveries, federation.DeliveryResult{Target: target, Status: status})
		}
		sort.Slice(result.Deliveries, func(i, j int) bool { return result.Deliveries[i].Target < result.Deliveries[j].Target })
		return result, nil
	default:
		return federation.AgentFrameResult{}, fmt.Errorf("unsupported agent frame type %q", frame.Type)
	}
}

// InheritedConnectorIdentity reads only daemon-issued internal launch metadata.
// An ordinary native session has none and therefore remains a bare connector.
func InheritedConnectorIdentity(product string) LocalControlIdentity {
	if inherited := strings.TrimSpace(os.Getenv(InternalProductEnvironment)); inherited != "" {
		product = inherited
	}
	return LocalControlIdentity{
		Role: LocalControlConnector, Product: strings.TrimSpace(product),
		AttachmentID: strings.TrimSpace(os.Getenv(InternalAttachmentIDEnvironment)),
		SessionID:    strings.TrimSpace(os.Getenv(InternalSessionIDEnvironment)),
		Capability:   strings.TrimSpace(os.Getenv(InternalCapabilityEnvironment)),
	}
}

// InheritedLauncherIdentity carries the same daemon-issued attachment
// capability through a short-lived public lane CLI. The daemon still derives
// authority from the exact attachment record; the CLI cannot supply a parent
// attachment in its lane payload.
func InheritedLauncherIdentity(product string) LocalControlIdentity {
	identity := InheritedConnectorIdentity(product)
	identity.Role = LocalControlLauncher
	return identity
}

// ForwardMCP relays one MCP method to the current daemon generation and returns its complete decision.
func ForwardMCP(ctx context.Context, identity LocalControlIdentity, method string, params json.RawMessage) (MCPForwardResult, error) {
	response, err := QueryLocalControl(ctx, identity, "mcp.forward", MCPForwardRequest{Method: method, Params: params})
	if err != nil {
		return MCPForwardResult{}, err
	}
	var result MCPForwardResult
	if err := decodeStrictControlFrame(response.Result, &result); err != nil {
		return MCPForwardResult{}, fmt.Errorf("decode daemon MCP decision: %w", err)
	}
	if len(result.Result) == 0 && result.Error == nil {
		return MCPForwardResult{}, errors.New("daemon returned an empty MCP decision")
	}
	return result, nil
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
