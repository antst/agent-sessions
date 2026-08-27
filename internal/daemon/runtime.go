package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/statestore"
)

// AdmissionState controls whether the host accepts new workflow operations.
type AdmissionState string

const (
	// AdmissionClosed identifies a runtime that accepts no work.
	AdmissionClosed AdmissionState = "closed"
	// AdmissionRecovering identifies a runtime reconstructing durable work.
	AdmissionRecovering AdmissionState = "recovering"
	// AdmissionReady identifies a runtime accepting authorized work.
	AdmissionReady AdmissionState = "ready"
	// AdmissionStopping identifies a runtime draining accepted work.
	AdmissionStopping AdmissionState = "stopping"
)

// RecoveryStage identifies one canonical ordered reconstruction boundary.
type RecoveryStage string

const (
	// RecoveryValidateAuthority validates roots and exclusive daemon authority.
	RecoveryValidateAuthority RecoveryStage = "validate_authority"
	// RecoveryLoadConfiguration validates the canonical host configuration.
	RecoveryLoadConfiguration RecoveryStage = "load_configuration"
	// RecoveryTransactions resolves incomplete authority transactions.
	RecoveryTransactions RecoveryStage = "recover_transactions"
	// RecoveryCatalog opens the session catalog and global group rules.
	RecoveryCatalog RecoveryStage = "open_catalog"
	// RecoveryAttachments loads durable attachments, lanes, turns, and debt.
	RecoveryAttachments RecoveryStage = "load_attachments_and_lanes"
	// RecoveryNativeActors corroborates exact external vendor actors.
	RecoveryNativeActors RecoveryStage = "reconcile_native_actors"
	// RecoveryRouting restores local delivery for ready products.
	RecoveryRouting RecoveryStage = "restore_local_routing"
	// RecoveryFederation reconnects the configured central hub.
	RecoveryFederation RecoveryStage = "reconnect_federation"
	// RecoveryCommitAuthority commits the generation before admission.
	RecoveryCommitAuthority RecoveryStage = "commit_authority"
	// RecoverySyntheticService publishes corroborated compatibility state last.
	RecoverySyntheticService RecoveryStage = "publish_synthetic_service"
)

var orderedRecoveryStages = [...]RecoveryStage{
	RecoveryValidateAuthority,
	RecoveryLoadConfiguration,
	RecoveryTransactions,
	RecoveryCatalog,
	RecoveryAttachments,
	RecoveryNativeActors,
	RecoveryRouting,
	RecoveryFederation,
	RecoveryCommitAuthority,
	RecoverySyntheticService,
}

// RecoveryHook reconstructs one in-process component before admission opens.
type RecoveryHook func(context.Context, *Runtime) error

// RuntimeOptions supplies exact authority and component hooks to one runtime.
type RuntimeOptions struct {
	Paths              ProductionPaths
	Configuration      DaemonConfig
	State              *StateStore
	RuntimeVersion     string
	RuntimeIdentity    string
	PID                int
	ProcStart          string
	StrongStart        string
	ServiceManager     string
	ServiceUnit        string
	Now                func() time.Time
	RecoveryHooks      map[RecoveryStage]RecoveryHook
	AttachmentAdapters map[string]AttachmentAdapter
	DeliveryAdapters   map[string]DeliveryAdapter
	DeliveryPreflight  func(context.Context, DeliveryRequest) error
	ObserveDelivery    func(DeliveryObservation)
}

// Runtime is the in-process composition root for every host responsibility.
// It owns one generation and one admission state; embedded product, routing,
// lane, and federation components attach through recovery hooks rather than
// creating child Agent Sessions authorities.
type Runtime struct {
	options RuntimeOptions

	mu             sync.RWMutex
	admission      AdmissionState
	generation     uint64
	stateRevision  statestore.Revision
	startedAt      int64
	completedStage []RecoveryStage
	attachments    *AttachmentRegistry
	deliveries     *DeliveryEngine
	mcp            *MCPService
}

// NewRuntime constructs one closed host composition root.
func NewRuntime(options RuntimeOptions) (*Runtime, error) {
	if options.State == nil {
		return nil, errors.New("daemon runtime requires a state store")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PID == 0 {
		options.PID = os.Getpid()
	}
	if options.Paths.ControlEndpoint == "" || options.RuntimeVersion == "" || options.RuntimeIdentity == "" ||
		options.ProcStart == "" || options.StrongStart == "" {
		return nil, errors.New("daemon runtime requires exact path, version, and process identity")
	}
	if err := options.Configuration.Validate(options.Paths); err != nil {
		return nil, fmt.Errorf("validate daemon runtime configuration: %w", err)
	}
	return &Runtime{options: options, admission: AdmissionClosed}, nil
}

// Admission returns the current admission state.
func (runtime *Runtime) Admission() AdmissionState {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.admission
}

// Generation returns the current committed generation candidate.
func (runtime *Runtime) Generation() uint64 {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.generation
}

// CompletedRecoveryStages returns an independent ordered recovery snapshot.
func (runtime *Runtime) CompletedRecoveryStages() []RecoveryStage {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return append([]RecoveryStage(nil), runtime.completedStage...)
}

// Start recovers one generation and opens admission only after authority commits.
func (runtime *Runtime) Start(ctx context.Context) error {
	runtime.mu.Lock()
	if runtime.admission != AdmissionClosed {
		runtime.mu.Unlock()
		return fmt.Errorf("daemon runtime cannot start from admission state %q", runtime.admission)
	}
	runtime.admission = AdmissionRecovering
	runtime.completedStage = nil
	runtime.startedAt = runtime.options.Now().UnixMilli()
	runtime.mu.Unlock()

	previous, revision, err := runtime.options.State.ReadRuntime(ctx)
	switch {
	case err == nil:
		// Starting/recovering generations have never opened admission and are
		// therefore retryable candidates, not consumed committed authorities.
		// Reuse their generation after a failed recovery or process crash.
		if previous.State == HostRuntimeStarting || previous.State == HostRuntimeRecovering {
			runtime.generation = previous.Generation
		} else {
			runtime.generation = previous.Generation + 1
		}
		runtime.stateRevision = revision
	case os.IsNotExist(err):
		runtime.generation = 1
		runtime.stateRevision = 0
	default:
		runtime.closeAdmission()
		return fmt.Errorf("load prior daemon authority: %w", err)
	}

	if err := runtime.commitLifecycle(ctx, HostRuntimeStarting, 0); err != nil {
		runtime.closeAdmission()
		return err
	}
	if err := runtime.commitLifecycle(ctx, HostRuntimeRecovering, 0); err != nil {
		runtime.closeAdmission()
		return err
	}
	for _, stage := range orderedRecoveryStages {
		if err := runtime.recoverOwnedStage(ctx, stage); err != nil {
			runtime.closeAdmission()
			return fmt.Errorf("recovery stage %s: %w", stage, err)
		}
		if hook := runtime.options.RecoveryHooks[stage]; hook != nil {
			if err := hook(ctx, runtime); err != nil {
				runtime.closeAdmission()
				return fmt.Errorf("recovery stage %s: %w", stage, err)
			}
		}
		runtime.mu.Lock()
		runtime.completedStage = append(runtime.completedStage, stage)
		runtime.mu.Unlock()
	}
	committedAt := runtime.options.Now().UnixMilli()
	if err := runtime.commitLifecycle(ctx, HostRuntimeReady, committedAt); err != nil {
		// Atomic rename can make the candidate record visible even when the
		// parent-directory durability acknowledgement fails. Revoke that
		// visible Ready state before returning a failed start.
		runtime.revokeFailedReady(ctx)
		runtime.closeAdmission()
		return err
	}
	runtime.mu.Lock()
	runtime.admission = AdmissionReady
	runtime.mu.Unlock()
	return nil
}

func (runtime *Runtime) recoverOwnedStage(ctx context.Context, stage RecoveryStage) error {
	switch stage {
	case RecoveryAttachments:
		registry, err := NewAttachmentRegistry(AttachmentRegistryOptions{
			State: runtime.options.State, Generation: runtime.generation,
			HostID: runtime.options.Configuration.HostID, Now: runtime.options.Now,
			Adapters: runtime.options.AttachmentAdapters,
		})
		if err != nil {
			return err
		}
		runtime.mu.Lock()
		runtime.attachments = registry
		runtime.mu.Unlock()
	case RecoveryNativeActors:
		registry := runtime.attachmentRegistry()
		if registry == nil {
			return errors.New("attachment registry was not recovered")
		}
		return registry.Reconcile(ctx)
	case RecoveryRouting:
		registry := runtime.attachmentRegistry()
		if registry == nil {
			return errors.New("attachment registry was not recovered")
		}
		engine, err := NewDeliveryEngine(DeliveryEngineOptions{
			State: runtime.options.State, Attachments: registry,
			Adapters: runtime.options.DeliveryAdapters, Now: runtime.options.Now,
			Preflight: runtime.options.DeliveryPreflight, Observe: runtime.options.ObserveDelivery,
		})
		if err != nil {
			return err
		}
		mcp, err := newMCPService(registry, engine)
		if err != nil {
			return err
		}
		runtime.mu.Lock()
		runtime.deliveries = engine
		runtime.mcp = mcp
		runtime.mu.Unlock()
	case RecoveryValidateAuthority, RecoveryLoadConfiguration, RecoveryTransactions, RecoveryCatalog,
		RecoveryFederation, RecoveryCommitAuthority, RecoverySyntheticService:
		// These stages are owned by their existing hooks or lifecycle commit.
	}
	return nil
}

func (runtime *Runtime) attachmentRegistry() *AttachmentRegistry {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.attachments
}

func (runtime *Runtime) deliveryEngine() *DeliveryEngine {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.deliveries
}

func (runtime *Runtime) mcpService() *MCPService {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.mcp
}

// Stop closes admission and commits the stopping lifecycle state.
func (runtime *Runtime) Stop(ctx context.Context) error {
	runtime.mu.Lock()
	if runtime.admission == AdmissionClosed {
		runtime.mu.Unlock()
		return nil
	}
	runtime.admission = AdmissionStopping
	runtime.mu.Unlock()
	committedAt := runtime.options.Now().UnixMilli()
	if err := runtime.commitLifecycle(ctx, HostRuntimeStopping, committedAt); err != nil {
		return err
	}
	runtime.closeAdmission()
	return nil
}

// Run starts, waits for cancellation, and performs bounded foreground shutdown.
func (runtime *Runtime) Run(ctx context.Context) error {
	if err := runtime.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.Stop(stopContext); err != nil {
		return err
	}
	return nil
}

func (runtime *Runtime) commitLifecycle(ctx context.Context, state HostRuntimeState, committedAt int64) error {
	record := HostRuntimeRecord{
		SchemaVersion: HostRuntimeSchemaVersion, Generation: runtime.generation,
		RuntimeVersion: runtime.options.RuntimeVersion, RuntimeIdentity: runtime.options.RuntimeIdentity,
		HostID: runtime.options.Configuration.HostID, HostName: runtime.options.Configuration.HostName,
		PID: runtime.options.PID, ProcStart: runtime.options.ProcStart, StrongStart: runtime.options.StrongStart,
		ControlEndpoint: runtime.options.Paths.ControlEndpoint,
		ServiceManager:  runtime.options.ServiceManager, ServiceUnit: runtime.options.ServiceUnit,
		StartedAt: runtime.startedAt, CommittedAt: committedAt, State: state,
		StateRevision: uint64(runtime.stateRevision + 1),
	}
	revision, err := runtime.options.State.CompareAndSwapRuntime(ctx, runtime.stateRevision, record)
	if err != nil {
		return fmt.Errorf("commit daemon %s authority: %w", state, err)
	}
	runtime.stateRevision = revision
	return nil
}

func (runtime *Runtime) closeAdmission() {
	runtime.mu.Lock()
	runtime.admission = AdmissionClosed
	runtime.mu.Unlock()
}

func (runtime *Runtime) revokeFailedReady(ctx context.Context) {
	record, revision, err := runtime.options.State.ReadRuntime(ctx)
	if err != nil || record.Generation != runtime.generation || record.State != HostRuntimeReady {
		return
	}
	record.State = HostRuntimeDebt
	record.StateRevision = uint64(revision + 1)
	if next, compareErr := runtime.options.State.CompareAndSwapRuntime(ctx, revision, record); compareErr == nil {
		runtime.stateRevision = next
	}
}
