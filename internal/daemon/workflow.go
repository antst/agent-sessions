package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		case "runtime.status", "runtime.doctor", "remove.inspect", "migration.inspect":
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
		case "peer.identity":
			result, err = registry.Select(ctx, AttachmentSelector{SessionID: principal.SessionID})
		case "peer.discover":
			engine := runtime.deliveryEngine()
			if engine == nil {
				return controlDispatchResult{}, &controlError{Code: "runtime_recovering", Message: "delivery authority is not ready", Retryable: true}
			}
			result, err = engine.Discover(ctx, principal.AttachmentID)
		case "peer.send", "peer.broadcast":
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

func decodeControlPayload(body json.RawMessage, target any) *controlError {
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &controlError{Code: "invalid_payload", Message: "request payload does not match the operation contract", Retryable: false}
	}
	return nil
}

func workflowResourceRevision(result any) string {
	switch value := result.(type) {
	case AttachmentRecord:
		return fmt.Sprintf("%d", value.Revision)
	case AttachmentPrepareResult:
		return fmt.Sprintf("%d", value.Attachment.Revision)
	case DeliveryRecord:
		return fmt.Sprintf("%d", value.Revision)
	default:
		return ""
	}
}

func controlFailure(err error) *controlError {
	switch {
	case errors.Is(err, ErrAttachmentNotAttested), errors.Is(err, ErrDeliveryUnauthorized):
		return &controlError{Code: "not_authorized", Message: err.Error(), Retryable: false}
	case errors.Is(err, ErrAttachmentNotFound), errors.Is(err, ErrDeliveryNotFound):
		return &controlError{Code: "not_found", Message: err.Error(), Retryable: false}
	case errors.Is(err, ErrAttachmentAmbiguous), errors.Is(err, ErrDeliveryIdempotencyConflict):
		return &controlError{Code: "conflict", Message: err.Error(), Retryable: false}
	case errors.Is(err, ErrAttachmentSelecting):
		return &controlError{Code: "selection_pending", Message: err.Error(), Retryable: true}
	default:
		return &controlError{Code: "adapter_failure", Message: err.Error(), Retryable: true}
	}
}
