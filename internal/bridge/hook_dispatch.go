package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	daemonpkg "github.com/antst/sessionbus/internal/daemon"
	"github.com/antst/sessionbus/internal/productcatalog"
)

// HookAttestation is exact non-model evidence for one native lifecycle event.
type HookAttestation = ConnectorAttestation

// HookDispatchConfig describes the real event surface for one product. An
// empty Events map is valid and means the product exposes no native hooks.
type HookDispatchConfig struct {
	Product    string
	Endpoint   string
	Events     map[string]bool
	Generation func(context.Context) (uint64, error)
	Attest     func(context.Context, string, json.RawMessage) (HookAttestation, error)
}

// HookDispatcher is a short-lived event transport; it owns no state or service lifetime.
type HookDispatcher struct {
	product string
	events  map[string]bool
	attest  func(context.Context, string, json.RawMessage) (HookAttestation, error)
	client  *daemonControlClient
}

type hookDispatchPayload struct {
	Product  string                   `json:"product"`
	Event    string                   `json:"event"`
	Input    json.RawMessage          `json:"input,omitempty"`
	Evidence daemonpkg.NativeEvidence `json:"evidence"`
}

// NewHookDispatcher validates one product-owned hook surface.
func NewHookDispatcher(config HookDispatchConfig) (*HookDispatcher, error) {
	if _, ok := productcatalog.ByID(config.Product); !ok {
		return nil, fmt.Errorf("hook dispatcher product %q is unsupported", config.Product)
	}
	client, err := newDaemonControlClient(config.Endpoint, config.Generation)
	if err != nil {
		return nil, fmt.Errorf("configure hook dispatcher: %w", err)
	}
	events := make(map[string]bool, len(config.Events))
	for event, enabled := range config.Events {
		event = strings.TrimSpace(event)
		if event == "" {
			return nil, errors.New("hook dispatcher event is empty")
		}
		events[event] = enabled
	}
	return &HookDispatcher{product: config.Product, events: events, attest: config.Attest, client: client}, nil
}

// Dispatch forwards one supported, exactly attested event. Unsupported and bare
// events are silent successful no-ops with no daemon request.
func (d *HookDispatcher) Dispatch(ctx context.Context, event string, input json.RawMessage) (json.RawMessage, error) {
	event = strings.TrimSpace(event)
	if !d.events[event] {
		return nil, nil
	}
	if d.attest == nil {
		return nil, nil
	}
	attestation, err := d.attest(ctx, event, append(json.RawMessage(nil), input...))
	if err != nil || strings.TrimSpace(attestation.AttachmentID) == "" {
		// Native attestation failure is the expected bare-session boundary.
		//nolint:nilerr // Hooks must remain silent and successful outside managed attachments.
		return nil, nil
	}
	payload, err := json.Marshal(hookDispatchPayload{
		Product: d.product, Event: event, Input: append(json.RawMessage(nil), input...),
		Evidence: cloneRelayEvidence(attestation.Evidence),
	})
	if err != nil {
		return nil, errors.New("encode managed hook event failed")
	}
	requestID := randomID()
	response, err := d.client.call(ctx, daemonpkg.ControlRequest{
		ID: requestID, Role: daemonpkg.RoleHook, Operation: "hook.event", IdempotencyKey: requestID,
		AttachmentID: attestation.AttachmentID, Capability: attestation.Capability, Payload: json.RawMessage(payload),
	})
	if err != nil {
		return nil, errors.New("agent sessions daemon is unavailable")
	}
	if response.Error != nil {
		return nil, errors.New(response.Error.Message)
	}
	if !response.OK {
		return nil, errors.New("managed hook event was not accepted")
	}
	if len(response.Payload) == 0 {
		return nil, nil
	}
	if !json.Valid(response.Payload) {
		return nil, errors.New("managed hook response is invalid")
	}
	return append(json.RawMessage(nil), response.Payload...), nil
}
