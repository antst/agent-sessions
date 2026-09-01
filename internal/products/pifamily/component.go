package pifamily

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productruntime"
)

// ComponentSender is implemented by component.Broker. Keeping this narrow lets
// product tests exercise typed operations without owning broker persistence or
// authentication.
type ComponentSender interface {
	Send(bindingID string, frameType component.FrameType, operationID string, payload any) error
}

type ComponentRenamer interface {
	RenameSession(context.Context, string, string, component.SessionRenameRequest) (component.SessionRename, error)
}

// BindingSource exposes only authenticated, secret-free live binding evidence.
type BindingSource interface {
	Bindings() []component.BindingView
}

type ParentToolCall struct {
	ProductID       string
	AttachmentID    string
	NativeSessionID string
	Operation       string
	Arguments       json.RawMessage
}

type ParentToolHandler interface {
	HandleParentTool(context.Context, ParentToolCall) (json.RawMessage, error)
}

type ComponentRuntimeConfig struct {
	Quirks   Quirks
	Sender   ComponentSender
	Renamer  ComponentRenamer
	Bindings BindingSource
	Tools    ParentToolHandler
	Now      func() time.Time
}

// ComponentRuntime owns correlation and product-native frame semantics only.
// Durable attachment/session/delivery admission is completed by the central
// componentruntime.Authority before this post-admission observer is invoked.
// This runtime never calls Authority and has no fallback/Next handler.
type ComponentRuntime struct {
	config ComponentRuntimeConfig

	mu              sync.Mutex
	sessions        map[string]componentIdentity // attachment id -> live identity
	pendingDelivery map[string]*pendingDelivery
	pendingTools    map[string]*pendingTool
	operation       atomic.Uint64
}

type componentIdentity struct {
	bindingID       string
	nativeSessionID string
	generation      uint64
	eventSequence   uint64
	state           string
}

type pendingDelivery struct {
	attachmentID    string
	nativeSessionID string
	result          chan deliveryResult
}

type deliveryResult struct {
	acceptance productruntime.NativeAcceptance
	err        error
}

type pendingTool struct {
	cancel context.CancelFunc
}

const (
	// Leave ample room for the component envelope and stay below the shared
	// transport's string/frame limits regardless of result shape.
	maxParentToolResultBytes   = 64 << 10
	maxParentToolResultNesting = 24
)

func NewComponentRuntime(config ComponentRuntimeConfig) (*ComponentRuntime, error) {
	if err := config.Quirks.Validate(); err != nil {
		return nil, err
	}
	if config.Sender == nil || config.Bindings == nil {
		return nil, errors.New("Pi-family component runtime requires a broker sender and binding source")
	}
	if config.Renamer == nil {
		config.Renamer, _ = config.Sender.(ComponentRenamer)
	}
	if config.Renamer == nil {
		return nil, errors.New("Pi-family component runtime requires broker-correlated native rename")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ComponentRuntime{
		config: config, sessions: make(map[string]componentIdentity),
		pendingDelivery: make(map[string]*pendingDelivery),
		pendingTools:    make(map[string]*pendingTool),
	}, nil
}

func (runtime *ComponentRuntime) ProductID() string {
	if runtime == nil {
		return ""
	}
	return runtime.config.Quirks.ProductID
}

func (runtime *ComponentRuntime) HandleComponentFrame(ctx context.Context, binding component.BindingView, frame component.Frame) error {
	if binding.ProductID != runtime.config.Quirks.ProductID || binding.BindingID == "" || binding.AttachmentID == "" {
		return fmt.Errorf("%w: component binding is for the wrong product", productruntime.ErrUnauthorized)
	}
	switch frame.Type {
	case component.TypeSessionAnnounce:
		var value component.SessionAnnounce
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		if value.BindingID != binding.BindingID {
			return fmt.Errorf("%w: announce binding id mismatch", productruntime.ErrUnauthorized)
		}
		return runtime.recordIdentity(binding, value.NativeSessionID, value.ProductEventSeq)
	case component.TypeSessionRebind:
		var value component.SessionRebind
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		return runtime.rebindIdentity(binding, value)
	case component.TypeSessionRename:
		var value component.SessionRename
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		return runtime.recordRename(binding, value)
	case component.TypeSessionState:
		var value component.SessionState
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		return runtime.recordState(binding, value)
	case component.TypeSessionClose:
		var value component.SessionClose
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		return runtime.recordClose(binding, value)
	case component.TypeDeliveryAccept:
		var value component.DeliveryAccept
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		if frame.ID != value.DeliveryID {
			return fmt.Errorf("%w: delivery acceptance frame id does not match its operation", productruntime.ErrProtocol)
		}
		return runtime.completeDelivery(binding, value)
	case component.TypeDeliveryReject:
		var value component.DeliveryReject
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		if frame.ID != value.DeliveryID {
			return fmt.Errorf("%w: delivery rejection frame id does not match its operation", productruntime.ErrProtocol)
		}
		return runtime.rejectDelivery(binding, value)
	case component.TypeToolCall:
		var value component.ToolCall
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		if frame.ID != value.CallID {
			return fmt.Errorf("%w: parent tool frame id does not match its operation", productruntime.ErrProtocol)
		}
		return runtime.startTool(ctx, binding, value)
	case component.TypeToolCancel:
		var value component.ToolCancel
		if err := frame.PayloadInto(&value); err != nil {
			return err
		}
		if frame.ID != value.CallID {
			return fmt.Errorf("%w: parent tool cancellation frame id does not match its operation", productruntime.ErrProtocol)
		}
		return runtime.cancelTool(binding, value)
	case component.TypeTurnEvent, component.TypeHeartbeat:
		return runtime.requireCurrentBinding(binding)
	default:
		return fmt.Errorf("%w: Pi-family component cannot handle %s", productruntime.ErrProtocol, frame.Type)
	}
}

func (runtime *ComponentRuntime) Deliver(ctx context.Context, attachment daemon.ManagedAttachment, request productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	if attachment.Product != runtime.config.Quirks.ProductID || attachment.State != "attached" || attachment.NativeSessionID == "" {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: delivery destination is not an exact attached native session", productruntime.ErrStale)
	}
	if request.DeliveryID == "" || !validDeliveryMode(request.Mode) || !validJSONObject(request.Body) {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: invalid component delivery", productruntime.ErrProtocol)
	}
	binding, err := runtime.liveBinding(attachment.ID, attachment.NativeSessionID)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	pending := &pendingDelivery{attachmentID: attachment.ID, nativeSessionID: attachment.NativeSessionID, result: make(chan deliveryResult, 1)}
	runtime.mu.Lock()
	if _, exists := runtime.pendingDelivery[request.DeliveryID]; exists {
		runtime.mu.Unlock()
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: delivery id is already outstanding", productruntime.ErrAmbiguousSession)
	}
	runtime.pendingDelivery[request.DeliveryID] = pending
	runtime.mu.Unlock()
	payload := component.DeliveryPresent{DeliveryID: request.DeliveryID, ReceiptID: request.ReceiptID, Mode: string(request.Mode), Body: append(json.RawMessage(nil), request.Body...)}
	if err := runtime.config.Sender.Send(binding.BindingID, component.TypeDeliveryPresent, request.DeliveryID, payload); err != nil {
		runtime.dropDelivery(request.DeliveryID, pending)
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: present component delivery: %v", productruntime.ErrUnavailable, err)
	}
	select {
	case <-ctx.Done():
		runtime.dropDelivery(request.DeliveryID, pending)
		return productruntime.NativeAcceptance{}, ctx.Err()
	case result := <-pending.result:
		return result.acceptance, result.err
	}
}

func (runtime *ComponentRuntime) Rename(ctx context.Context, attachment daemon.ManagedAttachment, name string) (productruntime.NativeName, error) {
	name = strings.TrimSpace(name)
	if ctx == nil || name == "" || len([]byte(name)) > 1024 || strings.ContainsRune(name, 0) {
		return productruntime.NativeName{}, fmt.Errorf("%w: native session name is invalid", productruntime.ErrNativeRejected)
	}
	if attachment.Product != runtime.config.Quirks.ProductID || attachment.State != "attached" || attachment.NativeSessionID == "" {
		return productruntime.NativeName{}, fmt.Errorf("%w: rename destination is stale", productruntime.ErrStale)
	}
	binding, err := runtime.liveBinding(attachment.ID, attachment.NativeSessionID)
	if err != nil {
		return productruntime.NativeName{}, err
	}
	operationID := fmt.Sprintf("%s%d", component.DaemonRenameOperationPrefix, runtime.operation.Add(1))
	result, err := runtime.config.Renamer.RenameSession(ctx, binding.BindingID, operationID, component.SessionRenameRequest{
		NativeSessionID: attachment.NativeSessionID, RequestedName: name,
	})
	if err != nil {
		return productruntime.NativeName{}, mapRenameError(err)
	}
	if result.NativeSessionID != attachment.NativeSessionID || result.NativeName != name || result.ProductEventSeq == 0 {
		return productruntime.NativeName{}, fmt.Errorf("%w: component rename changed the exact native result", productruntime.ErrProtocol)
	}
	return productruntime.NativeName{Applied: result.NativeName, NativeConfirmed: true}, nil
}

func mapRenameError(err error) error {
	var renameError *component.RenameError
	if !errors.As(err, &renameError) {
		return fmt.Errorf("%w: native rename unavailable", productruntime.ErrUnavailable)
	}
	detail := productruntime.NewRedactedString(renameError.Detail)
	switch renameError.Category {
	case component.RenameTimedOut:
		return fmt.Errorf("%w: %s", productruntime.ErrTimedOut, detail.String())
	case component.RenameNativeRejected:
		return fmt.Errorf("%w: %s", productruntime.ErrNativeRejected, detail.String())
	case component.RenameProtocol:
		return fmt.Errorf("%w: %s", productruntime.ErrProtocol, detail.String())
	default:
		return fmt.Errorf("%w: %s", productruntime.ErrUnavailable, detail.String())
	}
}

func (runtime *ComponentRuntime) recordIdentity(binding component.BindingView, nativeSessionID string, sequence uint64) error {
	if strings.TrimSpace(nativeSessionID) == "" || binding.Generation == 0 || sequence == 0 {
		return fmt.Errorf("%w: session announce omitted exact identity evidence", productruntime.ErrProtocol)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	current, exists := runtime.sessions[binding.AttachmentID]
	if exists && current.nativeSessionID != nativeSessionID {
		return fmt.Errorf("%w: session announce changed the native identity without rebind", productruntime.ErrAmbiguousSession)
	}
	if exists {
		if binding.Generation < current.generation || binding.Generation == current.generation && binding.BindingID != current.bindingID {
			return fmt.Errorf("%w: session announce changed the generation-local binding", productruntime.ErrStale)
		}
		if binding.Generation == current.generation && sequence <= current.eventSequence {
			return fmt.Errorf("%w: product event sequence did not advance", productruntime.ErrStale)
		}
	}
	runtime.sessions[binding.AttachmentID] = componentIdentity{
		bindingID: binding.BindingID, nativeSessionID: nativeSessionID, generation: binding.Generation,
		eventSequence: sequence, state: current.state,
	}
	return nil
}

func (runtime *ComponentRuntime) rebindIdentity(binding component.BindingView, value component.SessionRebind) error {
	if value.BindingID != binding.BindingID {
		return fmt.Errorf("%w: rebind binding id mismatch", productruntime.ErrUnauthorized)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	current, exists := runtime.sessions[binding.AttachmentID]
	if !exists || current.bindingID != binding.BindingID || current.generation != binding.Generation ||
		current.nativeSessionID != value.OldNativeSessionID || value.ProductEventSeq <= current.eventSequence {
		return fmt.Errorf("%w: session rebind does not continue the exact live identity", productruntime.ErrStale)
	}
	current.nativeSessionID = value.NewNativeSessionID
	current.eventSequence = value.ProductEventSeq
	runtime.sessions[binding.AttachmentID] = current
	return nil
}

func (runtime *ComponentRuntime) recordRename(binding component.BindingView, value component.SessionRename) error {
	if !component.ValidNativeTitleObservation(value.NativeName) {
		return fmt.Errorf("%w: rename carries an unsafe native title", productruntime.ErrProtocol)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	current, exists := runtime.sessions[binding.AttachmentID]
	if !exists || current.bindingID != binding.BindingID || current.generation != binding.Generation ||
		current.nativeSessionID != value.NativeSessionID || value.ProductEventSeq <= current.eventSequence {
		return fmt.Errorf("%w: rename does not match the exact current session", productruntime.ErrStale)
	}
	current.eventSequence = value.ProductEventSeq
	runtime.sessions[binding.AttachmentID] = current
	return nil
}

func (runtime *ComponentRuntime) recordState(binding component.BindingView, value component.SessionState) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	current, exists := runtime.sessions[binding.AttachmentID]
	if !exists || current.bindingID != binding.BindingID || current.generation != binding.Generation ||
		current.nativeSessionID != value.NativeSessionID || value.ProductEventSeq <= current.eventSequence {
		return fmt.Errorf("%w: state event does not match the exact current session", productruntime.ErrStale)
	}
	current.eventSequence = value.ProductEventSeq
	current.state = value.State
	runtime.sessions[binding.AttachmentID] = current
	return nil
}

func (runtime *ComponentRuntime) recordClose(binding component.BindingView, value component.SessionClose) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	current, exists := runtime.sessions[binding.AttachmentID]
	if !exists || current.bindingID != binding.BindingID || current.generation != binding.Generation || current.nativeSessionID != value.NativeSessionID {
		return fmt.Errorf("%w: close does not match the exact current session", productruntime.ErrStale)
	}
	delete(runtime.sessions, binding.AttachmentID)
	return nil
}

func (runtime *ComponentRuntime) completeDelivery(binding component.BindingView, value component.DeliveryAccept) error {
	runtime.mu.Lock()
	pending := runtime.pendingDelivery[value.DeliveryID]
	current := runtime.sessions[binding.AttachmentID]
	if pending == nil || pending.attachmentID != binding.AttachmentID || pending.nativeSessionID != value.NativeSessionID || current.bindingID != binding.BindingID {
		runtime.mu.Unlock()
		return fmt.Errorf("%w: native delivery acceptance changed destination identity", productruntime.ErrAmbiguousSession)
	}
	delete(runtime.pendingDelivery, value.DeliveryID)
	runtime.mu.Unlock()
	pending.result <- deliveryResult{acceptance: productruntime.NativeAcceptance{
		NativeSessionID: value.NativeSessionID, NativeMessageID: value.NativeMessageID,
		AcceptedAt: time.UnixMilli(value.AcceptedAt).UTC(),
	}}
	return nil
}

func (runtime *ComponentRuntime) rejectDelivery(binding component.BindingView, value component.DeliveryReject) error {
	runtime.mu.Lock()
	pending := runtime.pendingDelivery[value.DeliveryID]
	current := runtime.sessions[binding.AttachmentID]
	if pending == nil || pending.attachmentID != binding.AttachmentID || pending.nativeSessionID != current.nativeSessionID || current.bindingID != binding.BindingID {
		runtime.mu.Unlock()
		return fmt.Errorf("%w: delivery rejection names a foreign operation", productruntime.ErrProtocol)
	}
	delete(runtime.pendingDelivery, value.DeliveryID)
	runtime.mu.Unlock()
	detail := productruntime.NewRedactedString(value.Detail)
	pending.result <- deliveryResult{err: fmt.Errorf("%w: %s", productruntime.ErrNativeRejected, detail.String())}
	return nil
}

func (runtime *ComponentRuntime) startTool(ctx context.Context, binding component.BindingView, value component.ToolCall) error {
	if runtime.config.Tools == nil {
		return fmt.Errorf("%w: Pi-family parent tool handler is unavailable", productruntime.ErrUnavailable)
	}
	runtime.mu.Lock()
	identity, exists := runtime.sessions[binding.AttachmentID]
	if !exists || identity.bindingID != binding.BindingID {
		runtime.mu.Unlock()
		return fmt.Errorf("%w: parent tool has no exact announced session", productruntime.ErrUnauthorized)
	}
	key := binding.BindingID + "\x00" + value.CallID
	toolCtx, cancel := context.WithCancel(ctx)
	pending := &pendingTool{cancel: cancel}
	if _, exists := runtime.pendingTools[key]; exists {
		runtime.mu.Unlock()
		cancel()
		return fmt.Errorf("%w: parent tool call id is already active", productruntime.ErrAmbiguousSession)
	}
	runtime.pendingTools[key] = pending
	runtime.mu.Unlock()
	go func() {
		result, err := runtime.config.Tools.HandleParentTool(toolCtx, ParentToolCall{
			ProductID: runtime.config.Quirks.ProductID, AttachmentID: binding.AttachmentID,
			NativeSessionID: identity.nativeSessionID, Operation: value.Operation,
			Arguments: append(json.RawMessage(nil), value.Arguments...),
		})
		runtime.mu.Lock()
		if runtime.pendingTools[key] == pending {
			delete(runtime.pendingTools, key)
		}
		runtime.mu.Unlock()
		cancel()
		payload := component.ToolResult{CallID: value.CallID}
		if err == nil && validParentToolResult(result) {
			payload.Success = true
			payload.Result = result
		} else {
			payload.Success = false
			payload.Category = component.CategoryProtocol
			if errors.Is(err, productruntime.ErrUnauthorized) {
				payload.Category = component.CategoryUnauthorized
			}
			if err == nil {
				payload.Detail = "parent tool result is invalid or exceeds its bound"
			} else {
				payload.Detail = productruntime.NewRedactedString(err.Error()).String()
			}
		}
		if sendErr := runtime.config.Sender.Send(binding.BindingID, component.TypeToolResult, value.CallID, payload); sendErr != nil {
			// A serialization/transport rejection must not silently discard the
			// response. Retry once with a constant, guaranteed-small failure. A
			// genuinely dead binding may still reject the retry, but no product
			// result is reused or widened.
			fallback := component.ToolResult{
				CallID: value.CallID, Success: false, Category: component.CategoryInternal,
				Detail: "parent tool result delivery failed",
			}
			_ = runtime.config.Sender.Send(binding.BindingID, component.TypeToolResult, value.CallID, fallback)
		}
	}()
	return nil
}

func (runtime *ComponentRuntime) cancelTool(binding component.BindingView, value component.ToolCancel) error {
	key := binding.BindingID + "\x00" + value.CallID
	runtime.mu.Lock()
	pending := runtime.pendingTools[key]
	runtime.mu.Unlock()
	if pending == nil {
		return fmt.Errorf("%w: tool cancellation is stale", productruntime.ErrStale)
	}
	pending.cancel()
	return nil
}

func (runtime *ComponentRuntime) requireCurrentBinding(binding component.BindingView) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	identity, exists := runtime.sessions[binding.AttachmentID]
	if !exists || identity.bindingID != binding.BindingID || identity.generation != binding.Generation {
		return fmt.Errorf("%w: component frame is not for the current binding", productruntime.ErrStale)
	}
	return nil
}

func (runtime *ComponentRuntime) liveBinding(attachmentID, nativeSessionID string) (component.BindingView, error) {
	runtime.mu.Lock()
	identity, exists := runtime.sessions[attachmentID]
	runtime.mu.Unlock()
	if !exists || identity.nativeSessionID != nativeSessionID {
		return component.BindingView{}, fmt.Errorf("%w: component session identity is unavailable", productruntime.ErrStale)
	}
	for _, binding := range runtime.config.Bindings.Bindings() {
		if binding.BindingID == identity.bindingID && binding.AttachmentID == attachmentID &&
			binding.ProductID == runtime.config.Quirks.ProductID && binding.Generation == identity.generation {
			return binding, nil
		}
	}
	return component.BindingView{}, fmt.Errorf("%w: exact component binding is not live", productruntime.ErrUnavailable)
}

func (runtime *ComponentRuntime) identityForBinding(bindingID string) (componentIdentity, string, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	for attachmentID, identity := range runtime.sessions {
		if identity.bindingID == bindingID {
			return identity, attachmentID, true
		}
	}
	return componentIdentity{}, "", false
}

func (runtime *ComponentRuntime) dropDelivery(id string, expected *pendingDelivery) {
	runtime.mu.Lock()
	if runtime.pendingDelivery[id] == expected {
		delete(runtime.pendingDelivery, id)
	}
	runtime.mu.Unlock()
}

func validDeliveryMode(mode productruntime.DeliveryMode) bool {
	return mode == productruntime.DeliveryIdleWake || mode == productruntime.DeliveryBusySteer || mode == productruntime.DeliveryBusyFollow
}

func validJSONObject(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid([]byte(trimmed))
}

func validParentToolResult(result []byte) bool {
	if len(result) == 0 {
		return true
	}
	if len(result) > maxParentToolResultBytes || !json.Valid(result) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(result))
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return depth == 0
		}
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delimiter {
		case '{', '[':
			depth++
			if depth > maxParentToolResultNesting {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
}

var _ component.Handler = (*ComponentRuntime)(nil)
