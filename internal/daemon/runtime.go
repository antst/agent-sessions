package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"
)

const defaultRuntimeStateBytes int64 = 16 << 20

// RuntimeComponent is one long-lived in-process daemon component. Returning
// before daemon cancellation terminates the complete authority.
type RuntimeComponent func(context.Context) error

// LaneConnectorAuthorizer corroborates a capability-less native connector
// against one active lane. Most native lane processes inherit the daemon's
// one-turn capability directly. Codex is the exception: its MCP connector is
// hosted by the shared App Server, so authorization must use the exact native
// thread and App Server process evidence instead.
type LaneConnectorAuthorizer func(context.Context, Lane, NativeEvidence) error

// RuntimeConfig composes the minimal user-host daemon authority.
type RuntimeConfig struct {
	StateRoot               string
	MaxStateBytes           int64
	Release                 string
	Adapters                map[string]AttachmentAdapter
	LaneConnectorAuthorizer LaneConnectorAuthorizer
	Handler                 ControlHandler
	// Initialize restores generation-scoped durable authority after the
	// endpoint is bound but before any control request may observe readiness.
	Initialize func(*Runtime) error
	Components []RuntimeComponent
}

// Runtime owns the one local endpoint, durable store, attachments, and
// cancellable in-process components for one daemon generation.
type Runtime struct {
	state                   *StateStore
	attachments             *AttachmentEngine
	control                 *ControlServer
	generation              uint64
	handler                 ControlHandler
	laneConnectorAuthorizer LaneConnectorAuthorizer

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
	snapshot, err := state.Read()
	if err != nil {
		return nil, fmt.Errorf("read runtime state: %w", err)
	}
	if snapshot.Catalog.Host.Generation == math.MaxUint64 {
		return nil, errors.New("runtime generation is exhausted")
	}
	generation := snapshot.Catalog.Host.Generation + 1
	attachments, err := NewAttachmentEngine(state, generation, config.Adapters)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &Runtime{
		state: state, attachments: attachments, generation: generation, handler: config.Handler,
		laneConnectorAuthorizer: config.LaneConnectorAuthorizer,
		ctx:                     ctx, cancel: cancel, done: make(chan struct{}),
	}
	control, err := StartControlServer(ctx, config.StateRoot, generation, runtime.handleControl)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start runtime control authority: existing authority or unsafe endpoint: %w", err)
	}
	runtime.control = control

	catalog := snapshot.Catalog
	catalog.Host.User = currentRuntimeUser()
	catalog.Host.Host = currentRuntimeHost()
	catalog.Host.Generation = generation
	catalog.Host.Endpoint = control.Endpoint()
	catalog.Host.ServiceState = "running"
	if strings.TrimSpace(config.Release) != "" {
		catalog.Host.Release = strings.TrimSpace(config.Release)
	}
	if _, err := state.Commit(snapshot.Revision, catalog); err != nil {
		cancel()
		_ = control.Close()
		return nil, fmt.Errorf("commit running runtime generation: %w", err)
	}
	if config.Initialize != nil {
		if err := config.Initialize(runtime); err != nil {
			runtime.finish("failed", err, true)
			<-runtime.done
			return nil, fmt.Errorf("initialize runtime before readiness: %w", runtime.waitErr)
		}
	}
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

// Generation returns this runtime's exact durable generation.
func (r *Runtime) Generation() uint64 { return r.generation }

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

func (r *Runtime) finish(serviceState string, cause error, commit bool) {
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
		if commit {
			if err := r.commitServiceState(serviceState); err != nil {
				failures = append(failures, err)
			}
		}
		r.waitErr = errors.Join(failures...)
		close(r.done)
	})
}

// crashForTest models process death: resources close, but no terminal durable
// state is committed. The successor must recover from the still-running record.
func (r *Runtime) crashForTest(_ error) error {
	r.finish("", nil, false)
	<-r.done
	return r.waitErr
}

func (r *Runtime) commitServiceState(state string) error {
	snapshot, err := r.state.Read()
	if err != nil {
		return err
	}
	catalog := snapshot.Catalog
	if catalog.Host.Generation != r.generation {
		return errors.New("runtime generation changed before service-state commit")
	}
	catalog.Host.ServiceState = state
	_, err = r.state.Commit(snapshot.Revision, catalog)
	return err
}

func (r *Runtime) handleControl(ctx context.Context, request ControlRequest) (json.RawMessage, error) {
	if !r.ready.Load() {
		return nil, errors.New("runtime is not ready")
	}
	switch request.Operation {
	case "status", "doctor":
		return r.runtimeStatus(request.Operation)
	case "attachment.prepare", "attachment.adopt", "attachment.refresh", "attachment.detach", "attachment.rollback":
		return r.handleAttachmentControl(ctx, request)
	}
	if request.Role == RoleHook || request.Role == RoleConnector && request.Operation == "connector.call" {
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
	err := r.attachments.Authorize(
		r.ctx, request.AttachmentID, request.Capability, strings.TrimSpace(envelope.Product), envelope.Evidence,
	)
	if err == nil || request.Role != RoleConnector {
		return err
	}
	snapshot, readErr := r.state.Read()
	if readErr != nil {
		return InactiveControlError()
	}
	lane, ok := snapshot.Catalog.Lanes[request.AttachmentID]
	if !ok || lane.Product != strings.TrimSpace(envelope.Product) ||
		(lane.State != "preparing" && lane.State != "running") {
		return InactiveControlError()
	}
	if strings.TrimSpace(request.Capability) != "" {
		if !capabilityMatches(lane.CapabilityHash, request.Capability) {
			return InactiveControlError()
		}
		return nil
	}
	if r.laneConnectorAuthorizer == nil || r.laneConnectorAuthorizer(r.ctx, lane, envelope.Evidence) != nil {
		return InactiveControlError()
	}
	return nil
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

func currentRuntimeUser() string {
	if current, err := user.Current(); err == nil && current.Uid != "" {
		return current.Uid
	}
	return fmt.Sprintf("%d", os.Getuid())
}

func currentRuntimeHost() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "unknown"
	}
	return host
}
