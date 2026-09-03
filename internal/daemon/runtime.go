package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

const defaultRuntimeStateBytes int64 = 16 << 20

var nextRuntimeGeneration atomic.Uint64

// RuntimeComponent is one long-lived in-process daemon component. Returning
// before daemon cancellation terminates the complete authority.
type RuntimeComponent func(context.Context) error

// ProductDiagnosticsProvider returns live, metadata-only readiness states for
// one status or doctor request. Durable Host.ProductReadiness is legacy state
// and is never an authority for runtime diagnostics.
type ProductDiagnosticsProvider func(context.Context, string) (map[string]string, error)

// RuntimeConfig composes the minimal user-host daemon authority.
type RuntimeConfig struct {
	StateRoot                  string
	MaxStateBytes              int64
	Release                    string
	ReleaseIdentity            string
	Adapters                   map[string]AttachmentAdapter
	ProductDiagnosticsProvider ProductDiagnosticsProvider
	Handler                    ControlHandler
	Components                 []RuntimeComponent
}

// Runtime owns the one local endpoint, durable store, attachments, and
// cancellable in-process components for one daemon generation.
type Runtime struct {
	state                      *StateStore
	attachments                *AttachmentEngine
	control                    *ControlServer
	generation                 uint64
	hostID                     string
	release                    string
	releaseIdentity            string
	handler                    ControlHandler
	productDiagnosticsProvider ProductDiagnosticsProvider

	ctx    context.Context
	cancel context.CancelFunc
	ready  atomic.Bool

	componentWG sync.WaitGroup
	finishOnce  sync.Once
	done        chan struct{}
	waitErr     error
}

type runtimeAttachmentRequest struct {
	Attachment ManagedAttachment `json:"attachment"`
	ID         string            `json:"id"`
	Evidence   NativeEvidence    `json:"evidence"`
	Cause      string            `json:"cause"`
}

// StartRuntime binds the sole endpoint before changing durable runtime state.
// A duplicate authority therefore fails without replacing the live socket or
// committing a second generation.
func StartRuntime(parent context.Context, config RuntimeConfig) (*Runtime, error) {
	if parent == nil {
		return nil, errors.New("runtime parent context is nil")
	}
	if strings.TrimSpace(config.StateRoot) == "" {
		return nil, errors.New("runtime state root is empty")
	}
	if config.MaxStateBytes <= 0 {
		config.MaxStateBytes = defaultRuntimeStateBytes
	}
	state, err := OpenState(config.StateRoot, config.MaxStateBytes)
	if err != nil {
		return nil, fmt.Errorf("open runtime state: %w", err)
	}
	generation := nextRuntimeGeneration.Add(1)
	attachments, err := NewAttachmentEngine(generation, config.Adapters)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &Runtime{
		state: state, attachments: attachments, generation: generation,
		hostID: currentRuntimeHost(), release: strings.TrimSpace(config.Release),
		releaseIdentity: strings.TrimSpace(config.ReleaseIdentity), handler: config.Handler,
		productDiagnosticsProvider: config.ProductDiagnosticsProvider,
		ctx:                        ctx, cancel: cancel, done: make(chan struct{}),
	}
	control, err := StartControlServer(ctx, config.StateRoot, generation, runtime.releaseIdentity, runtime.handleControl)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start runtime control authority: existing authority or unsafe endpoint: %w", err)
	}
	runtime.control = control

	runtime.ready.Store(true)
	runtime.startComponents(config.Components)
	go func() {
		select {
		case <-parent.Done():
			runtime.finish("stopped", nil, true)
		case <-runtime.done:
		}
	}()
	return runtime, nil
}

// Endpoint returns the fixed bound local endpoint.
func (r *Runtime) Endpoint() string { return r.control.Endpoint() }

// Generation returns this process-local runtime incarnation.
func (r *Runtime) Generation() uint64 { return r.generation }

// HostID returns the identity derived by the running daemon.
func (r *Runtime) HostID() string { return r.hostID }

// Release returns the configured running release label.
func (r *Runtime) Release() string { return r.release }

// State returns the runtime's durable state store.
func (r *Runtime) State() *StateStore { return r.state }

// Attachments returns the shared attachment transaction engine.
func (r *Runtime) Attachments() *AttachmentEngine { return r.attachments }

// Close performs an explicit normal stop. It never restarts itself.
func (r *Runtime) Close() error {
	r.finish("stopped", nil, true)
	<-r.done
	return r.waitErr
}

// Wait blocks until explicit stop, parent cancellation, or component failure.
func (r *Runtime) Wait() error {
	<-r.done
	return r.waitErr
}

func (r *Runtime) startComponents(components []RuntimeComponent) {
	if len(components) == 0 {
		return
	}
	results := make(chan error, len(components))
	for _, component := range components {
		if component == nil {
			continue
		}
		r.componentWG.Add(1)
		go func(run RuntimeComponent) {
			defer r.componentWG.Done()
			results <- run(r.ctx)
		}(component)
	}
	go func() {
		select {
		case err := <-results:
			if err == nil && r.ctx.Err() == nil {
				err = errors.New("runtime component exited before cancellation")
			}
			if r.ctx.Err() == nil {
				r.finish("failed", err, true)
			}
		case <-r.done:
		}
	}()
}

func (r *Runtime) finish(_ string, cause error, _ bool) {
	r.finishOnce.Do(func() {
		r.ready.Store(false)
		r.cancel()
		var failures []error
		if cause != nil {
			failures = append(failures, cause)
		}
		if r.control != nil {
			if err := r.control.Close(); err != nil {
				failures = append(failures, err)
			}
		}
		r.componentWG.Wait()
		r.waitErr = errors.Join(failures...)
		close(r.done)
	})
}

// crashForTest models process death without a durable runtime record.
func (r *Runtime) crashForTest(_ error) error {
	r.finish("", nil, false)
	<-r.done
	return r.waitErr
}

func (r *Runtime) handleControl(ctx context.Context, request ControlRequest) (json.RawMessage, error) {
	if !r.ready.Load() {
		return nil, errors.New("runtime is not ready")
	}
	switch request.Operation {
	case "status", "doctor":
		return r.runtimeStatus(ctx, request.Operation)
	case "attachment.prepare", "attachment.adopt", "attachment.refresh", "attachment.detach", "attachment.rollback":
		return r.handleAttachmentControl(ctx, request)
	}
	if request.Role == RoleHook {
		if err := r.authorizeAttachmentRequest(request); err != nil {
			return nil, err
		}
	}
	if r.handler == nil {
		return nil, errors.New("runtime operation handler is unavailable")
	}
	return r.handler(ctx, request)
}

func (r *Runtime) handleAttachmentControl(ctx context.Context, request ControlRequest) (json.RawMessage, error) {
	var input runtimeAttachmentRequest
	if err := json.Unmarshal(request.Payload, &input); err != nil {
		return nil, errors.New("decode attachment control payload failed")
	}
	var (
		attachment ManagedAttachment
		err        error
	)
	switch request.Operation {
	case "attachment.prepare":
		attachment, err = r.attachments.Prepare(ctx, input.Attachment)
	case "attachment.adopt":
		attachment, err = r.attachments.Adopt(ctx, input.ID, input.Evidence)
	case "attachment.refresh":
		attachment, err = r.attachments.Refresh(ctx, input.ID)
	case "attachment.detach":
		attachment, err = r.attachments.Detach(ctx, input.ID, input.Cause)
	case "attachment.rollback":
		attachment, err = r.attachments.Rollback(ctx, input.ID, input.Cause)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"attachment": attachment})
}

func (r *Runtime) authorizeAttachmentRequest(request ControlRequest) error {
	var envelope struct {
		Product  string         `json:"product"`
		Evidence NativeEvidence `json:"evidence"`
	}
	if len(request.Payload) == 0 || json.Unmarshal(request.Payload, &envelope) != nil {
		return InactiveControlError()
	}
	return r.attachments.Authorize(
		r.ctx, request.AttachmentID, request.Capability, strings.TrimSpace(envelope.Product), envelope.Evidence,
	)
}

// CapabilityDigest returns the stored one-way identity of a daemon-minted capability.
func CapabilityDigest(capability string) string {
	digest := sha256.Sum256([]byte(capability))
	return hex.EncodeToString(digest[:])
}

func capabilityMatches(expectedHash, capability string) bool {
	expected, err := hex.DecodeString(expectedHash)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	actual := sha256.Sum256([]byte(capability))
	return subtle.ConstantTimeCompare(expected, actual[:]) == 1
}

func currentRuntimeHost() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "unknown"
	}
	return host
}
