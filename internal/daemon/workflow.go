package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/antst/agent-sessions/internal/federation"
)

// AttachmentDetachRequest unpublishes one exact prepared or attached native actor.
type AttachmentDetachRequest struct {
	AttachmentID string `json:"attachment_id"`
	Reason       string `json:"reason,omitempty"`
}

//nolint:gocyclo // Closed role-aware dispatch keeps every accepted operation auditable in one boundary.
func runtimeControlDispatch(runtime *Runtime) func(context.Context, controlPrincipal, controlRequest) (controlDispatchResult, *controlError) {
	admin := runtimeAdminDispatch(runtime)
	return func(ctx context.Context, principal controlPrincipal, request controlRequest) (controlDispatchResult, *controlError) {
		switch request.Operation {
		case "runtime.status", "runtime.doctor", "remove.inspect":
			return admin(ctx, principal, request)
		}
		registry := runtime.attachmentRegistry()
		if registry == nil {
			return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "attachment authority is not ready", Retryable: true}
		}
		var (
			result any
			err    error
		)
		switch request.Operation {
		case "mcp.forward":
			var payload MCPForwardRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			service := runtime.mcpService()
			if service == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "MCP authority is not ready", Retryable: true}
			}
			result = service.Forward(ctx, principal, payload)
		case "attachment.prepare":
			var payload AttachmentPrepareRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			var record AttachmentRecord
			var capability string
			var launch NativeLaunchPlan
			record, capability, launch, err = registry.PrepareInteractive(ctx, payload)
			result = AttachmentPrepareResult{Attachment: record, Capability: capability, Launch: launch}
		case "attachment.adopt":
			var payload AttachmentAdoptRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			result, err = registry.Adopt(ctx, payload)
		case "attachment.refresh":
			var payload AttachmentRefreshRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			result, err = registry.Refresh(ctx, payload)
		case "attachment.detach":
			var payload AttachmentDetachRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			result, err = registry.Detach(ctx, payload.AttachmentID, payload.Reason)
		case "attachment.lookup":
			if principal.Role != controlRoleLauncher {
				err = ErrAttachmentNotAttested
				break
			}
			var payload AttachmentLookupRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			result, err = registry.LookupManaged(ctx, payload)
		case "attachment.context":
			if !principal.Attested {
				err = ErrAttachmentNotAttested
				break
			}
			var payload AttachmentLookupRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			if payload.SessionID != "" && payload.SessionID != principal.SessionID {
				err = ErrAttachmentNotAttested
				break
			}
			var record AttachmentRecord
			record, err = registry.Select(ctx, AttachmentSelector{SessionID: principal.SessionID})
			if err == nil {
				result = attachmentParentContext(record)
			}
		case "peer.identity":
			if !principal.Attested {
				err = ErrAttachmentNotAttested
				break
			}
			result, err = registry.Select(ctx, AttachmentSelector{SessionID: principal.SessionID})
		case "peer.discover":
			if !principal.Attested {
				err = ErrAttachmentNotAttested
				break
			}
			engine := runtime.deliveryEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "delivery authority is not ready", Retryable: true}
			}
			result, err = engine.Discover(ctx, principal.AttachmentID)
		case "peer.send", "peer.broadcast":
			if !principal.Attested {
				err = ErrAttachmentNotAttested
				break
			}
			engine := runtime.deliveryEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "delivery authority is not ready", Retryable: true}
			}
			var payload DeliveryRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			payload.SourceAttachmentID = principal.AttachmentID
			switch {
			case request.Operation == "peer.broadcast":
				payload.Operation = DeliveryOperationBroadcast
			case len(payload.Targets) > 1:
				payload.Operation = DeliveryOperationMulticast
			default:
				payload.Operation = DeliveryOperationSend
			}
			result, err = engine.Accept(ctx, payload)
		case "lane.command":
			if !principal.Attested || principal.AttachmentID == "" {
				err = ErrAttachmentNotAttested
				break
			}
			engine := runtime.laneEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "lane authority is not ready", Retryable: true}
			}
			var payload LaneCommandRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			if strings.TrimSpace(payload.Host) != "" {
				runtime.mu.RLock()
				federated := runtime.federation
				runtime.mu.RUnlock()
				if federated == nil {
					return controlDispatchResult{}, &controlError{Code: "federation_unavailable", Message: "federation authority is not ready", Retryable: true}
				}
				result, err = federated.executeRemoteLaneCommand(ctx, engine, registry, principal.AttachmentID, payload)
			} else {
				result, err = executeLaneCommand(ctx, engine, registry, principal.AttachmentID, payload)
			}
		case "lane.start", "lane.resume", "lane.followup":
			engine := runtime.laneEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "lane authority is not ready", Retryable: true}
			}
			var payload LaneStartRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			if principal.Attested {
				payload.SourceAttachmentID = principal.AttachmentID
			}
			var lane LaneRecord
			var turn LaneTurnRecord
			lane, turn, err = engine.Start(ctx, payload)
			result = map[string]any{"lane": lane, "turn": turn}
		case "lane.status":
			engine := runtime.laneEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "lane authority is not ready", Retryable: true}
			}
			var payload LaneArchiveRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			if principal.Attested {
				payload.SourceAttachmentID = principal.AttachmentID
			}
			result, err = engine.Status(ctx, payload)
		case "lane.list":
			engine := runtime.laneEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "lane authority is not ready", Retryable: true}
			}
			var payload LaneListRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			if principal.Attested {
				payload.SourceAttachmentID = principal.AttachmentID
			}
			result, err = engine.List(ctx, payload)
		case "lane.interrupt", "lane.collect":
			engine := runtime.laneEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "lane authority is not ready", Retryable: true}
			}
			var payload LaneCollectRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			if principal.Attested {
				payload.SourceAttachmentID = principal.AttachmentID
			}
			if request.Operation == "lane.interrupt" {
				result, err = engine.Interrupt(ctx, payload)
			} else {
				result, err = engine.Collect(ctx, payload)
			}
		case "lane.archive":
			engine := runtime.laneEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "lane authority is not ready", Retryable: true}
			}
			var payload LaneArchiveRequest
			if failure := decodeControlPayload(request.Payload, &payload); failure != nil {
				return controlDispatchResult{}, failure
			}
			if principal.Attested {
				payload.SourceAttachmentID = principal.AttachmentID
			}
			result, err = engine.Archive(ctx, payload)
		default:
			return controlDispatchResult{}, &controlError{Code: "operation_unavailable", Message: "workflow operation is not implemented", Retryable: false}
		}
		if err != nil {
			return controlDispatchResult{}, controlFailure(err)
		}
		body, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return controlDispatchResult{}, &controlError{Code: "internal", Message: "encode workflow result", Retryable: true}
		}
		return controlDispatchResult{Result: body, ResourceRevision: workflowResourceRevision(result)}, nil
	}
}

func attachmentParentContext(record AttachmentRecord) federation.ParentContext {
	adapterPID := attachmentEvidenceInt(record.NativeActor, "pid")
	adapterStart := attachmentEvidenceString(record.NativeActor, "proc_start")
	adapterStrongStart := attachmentEvidenceString(record.NativeActor, "strong_start")
	ownerPID := attachmentEvidenceInt(record.NativeActor, "parent_pid")
	if ownerPID <= 1 {
		ownerPID = adapterPID
	}
	ownerStart := attachmentEvidenceString(record.NativeActor, "parent_proc_start")
	if ownerStart == "" {
		ownerStart = adapterStart
	}
	ownerStrongStart := attachmentEvidenceString(record.NativeActor, "parent_strong_start")
	if ownerStrongStart == "" {
		ownerStrongStart = adapterStrongStart
	}
	return federation.ParentContext{
		HostID: record.HostID, SessionID: record.SessionID, Product: record.Product,
		InstanceID: record.AttachmentID, Groups: append([]string(nil), record.Groups...),
		PermissionMode: record.PermissionMode, AdapterPID: adapterPID, AdapterProcStart: adapterStart,
		AdapterStrongStart: adapterStrongStart, AdapterSocket: attachmentEvidenceString(record.NativeActor, "socket"),
		PID: ownerPID, ProcStart: ownerStart, StrongStart: ownerStrongStart,
		QwenCapabilityDigest: attachmentEvidenceString(record.NativeActor, "capability_digest"),
	}
}

func attachmentEvidenceString(evidence map[string]any, key string) string {
	value, _ := evidence[key].(string)
	return value
}

func attachmentEvidenceInt(evidence map[string]any, key string) int {
	switch value := evidence[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}

func decodeControlPayload(body json.RawMessage, target any) *controlError {
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &controlError{Code: "invalid_payload", Message: "request payload does not match the operation contract", Retryable: false}
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return &controlError{Code: "invalid_payload", Message: "request payload contains trailing JSON", Retryable: false}
	}
	return nil
}

func workflowResourceRevision(result any) string {
	switch value := result.(type) {
	case AttachmentRecord:
		return fmt.Sprintf("%d", value.Revision)
	case AttachmentPrepareResult:
		return fmt.Sprintf("%d", value.Attachment.Revision)
	case ManagedAttachment:
		return value.SessionID
	case DeliveryRecord:
		return fmt.Sprintf("%d", value.Revision)
	case LaneRecord:
		return fmt.Sprintf("%d", value.Revision)
	case LaneTurnRecord:
		return fmt.Sprintf("%d", value.Revision)
	case LaneCollection:
		return fmt.Sprintf("%d", value.CollectionRevision)
	default:
		return ""
	}
}

func controlFailure(err error) *controlError {
	switch {
	case errors.Is(err, ErrAttachmentNotAttested), errors.Is(err, ErrDeliveryUnauthorized):
		return &controlError{Code: "not_authorized", Message: err.Error(), Retryable: false}
	case errors.Is(err, ErrAttachmentNotFound), errors.Is(err, ErrDeliveryNotFound), errors.Is(err, ErrLaneNotFound):
		return &controlError{Code: "not_found", Message: err.Error(), Retryable: false}
	case errors.Is(err, ErrAttachmentAmbiguous), errors.Is(err, ErrDeliveryIdempotencyConflict),
		errors.Is(err, ErrLaneIdempotencyConflict), errors.Is(err, ErrLaneArchived):
		return &controlError{Code: "conflict", Message: err.Error(), Retryable: false}
	case errors.Is(err, ErrAttachmentSelecting):
		return &controlError{Code: "selection_pending", Message: err.Error(), Retryable: true}
	default:
		return &controlError{Code: "adapter_failure", Message: err.Error(), Retryable: true}
	}
}
