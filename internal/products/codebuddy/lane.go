package codebuddy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

type laneRuntime struct {
	ref        productruntime.NativeSessionRef
	cwd        string
	permission NativePermission
	server     *productserver.OwnedServer
	client     *APIClient

	// nativeMu serializes mutating CodeBuddy protocol calls for this lane. It
	// must never be held while driver.mu is held: one slow native server must
	// not block unrelated lanes or access to the lane index.
	nativeMu     sync.Mutex
	waitMu       sync.Mutex
	stateMu      sync.Mutex
	job          *AgentJob
	active       map[string]turnObservation
	nextTurn     uint64
	archived     bool
	archiving    bool
	cleanupDebt  bool
	lastTurnID   string
	lastTerminal productruntime.NativeTerminal
	pending      *pendingDispatch
}

type pendingDispatch struct {
	receiptID string
	marker    string
	baseline  map[string]struct{}
}

type turnObservation struct {
	jobID       string
	baseline    int64
	seenRunning bool
}

type LaneDriver struct {
	config Config
	deps   productruntime.HostDeps
	mu     sync.Mutex
	lanes  map[string]*laneRuntime
}

func newLaneDriver(config Config, deps productruntime.HostDeps) *LaneDriver {
	return &LaneDriver{config: config, deps: deps, lanes: make(map[string]*laneRuntime)}
}

func NewLaneDriver(config Config, deps productruntime.HostDeps) (*LaneDriver, error) {
	normalized, err := normalizeConfig(config, deps)
	if err != nil {
		return nil, err
	}
	if deps.OwnedProcesses == nil || deps.Receipts == nil || deps.Generation == 0 || normalized.Recovery == nil {
		return nil, ErrInvalidConfiguration
	}
	return newLaneDriver(normalized, deps), nil
}

func (*LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{Steer: true, DurableResume: true, DeferredSessionBinding: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if err := driver.validateOpen(request); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	return driver.open(ctx, request)
}

func (driver *LaneDriver) open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	request.Cwd = filepath.Clean(request.Cwd)
	driver.mu.Lock()
	existing := driver.lanes[request.LaneID]
	driver.mu.Unlock()
	if existing != nil && !existing.isArchived() {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: lane is already open", productruntime.ErrAmbiguousSession)
	}

	permission, err := MapLanePermission(request.PermissionMode, driver.config.AllowSandboxBypass)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	endpoint, err := driver.config.Endpoints.Allocate(ctx)
	if err != nil {
		return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrUnavailable, err)
	}
	port, err := endpointPort(endpoint)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	password, err := driver.config.Secrets.Generate(ctx)
	if err != nil || password.Empty() {
		return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrUnavailable, err)
	}
	auth, err := productserver.NewBearerAuth(password)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	nativeSessionID := request.ResumeNativeID
	command, err := driver.laneCommand(request, nativeSessionID, port, password, permission)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	server, err := productserver.StartOwnedServer(ctx, productserver.OwnedServerConfig{
		Command: command, Endpoint: endpoint, Auth: auth, Limits: driver.config.Limits,
		Supervisor: driver.deps.OwnedProcesses,
		Ready: func(probeCtx context.Context, safe *productserver.Client) error {
			return wrapOwnedClient(safe).Health(probeCtx)
		},
	})
	if err != nil {
		if server != nil {
			return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrCleanupDebt, err)
		}
		return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrUnavailable, err)
	}
	runtime := &laneRuntime{
		ref: productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: nativeSessionID, Generation: driver.deps.Generation},
		cwd: request.Cwd, permission: permission, server: server, client: wrapOwnedClient(server.Client()), active: make(map[string]turnObservation),
	}
	if request.ResumeNativeID != "" {
		job, resumeErr := runtime.client.ResumeJob(ctx, request.ResumeNativeID, request.Cwd)
		if resumeErr == nil {
			resumeErr = runtime.validateJob(job, "")
		}
		if resumeErr != nil {
			closeErr := server.Close(context.Background())
			if closeErr != nil {
				return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrCleanupDebt, resumeErr, closeErr)
			}
			return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrUnsupportedRecovery, resumeErr)
		}
		runtime.job = &job
	}
	driver.mu.Lock()
	existing = driver.lanes[request.LaneID]
	if existing != nil && !existing.isArchived() {
		driver.mu.Unlock()
		closeErr := server.Close(context.Background())
		return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrAmbiguousSession, closeErr)
	}
	driver.lanes[request.LaneID] = runtime
	driver.mu.Unlock()
	return runtime.ref, nil
}

func (driver *LaneDriver) StartTurn(ctx context.Context, reference productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	runtime, err := driver.runtime(reference)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	permission, err := MapLanePermission(request.PermissionMode, driver.config.AllowSandboxBypass)
	if err != nil || permission.Mode != runtime.permission.Mode {
		if err == nil {
			err = productruntime.ErrUnsupportedPolicy
		}
		return productruntime.NativeTurnRef{}, err
	}
	prompt, err := readReceipt(driver.deps.Receipts, request.ReceiptID)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	runtime.nativeMu.Lock()
	defer runtime.nativeMu.Unlock()
	runtime.stateMu.Lock()
	if err := runtime.usableLocked(reference); err != nil {
		runtime.stateMu.Unlock()
		return productruntime.NativeTurnRef{}, err
	}
	if len(runtime.active) != 0 {
		runtime.stateMu.Unlock()
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: CodeBuddy lane already has an active turn", productruntime.ErrAmbiguousSession)
	}
	previous := runtime.job
	pending := runtime.pending
	firstBinding := previous == nil && reference.NativeSessionID == ""
	runtime.stateMu.Unlock()
	var job AgentJob
	baseline := int64(0)
	if previous == nil {
		if reference.NativeSessionID != "" {
			return productruntime.NativeTurnRef{}, fmt.Errorf("%w: bound lane has no exact CodeBuddy job", productruntime.ErrStale)
		}
		if pending != nil {
			if pending.receiptID != request.ReceiptID {
				return productruntime.NativeTurnRef{}, fmt.Errorf("%w: an earlier CodeBuddy dispatch is ambiguous", productruntime.ErrAmbiguousSession)
			}
			job, err = runtime.reconcilePendingDispatch(ctx, pending)
			if err != nil {
				return productruntime.NativeTurnRef{}, err
			}
		} else {
			jobs, listErr := runtime.client.ListJobs(ctx, runtime.cwd)
			if listErr != nil {
				return productruntime.NativeTurnRef{}, listErr
			}
			pending = &pendingDispatch{
				receiptID: request.ReceiptID,
				marker:    dispatchMarker(reference, request.ReceiptID),
				baseline:  jobIdentitySet(jobs),
			}
			runtime.stateMu.Lock()
			if stateErr := runtime.usableLocked(reference); stateErr != nil || runtime.job != nil || runtime.pending != nil {
				runtime.stateMu.Unlock()
				if stateErr != nil {
					return productruntime.NativeTurnRef{}, stateErr
				}
				return productruntime.NativeTurnRef{}, productruntime.ErrAmbiguousSession
			}
			runtime.pending = pending
			runtime.stateMu.Unlock()
			job, err = runtime.client.DispatchJob(ctx, DispatchJobRequest{
				Prompt: prompt, Cwd: runtime.cwd, PermissionMode: permission.Mode, Name: pending.marker,
			})
			if err != nil {
				if errors.Is(err, productruntime.ErrNativeRejected) || errors.Is(err, productruntime.ErrUnauthorized) {
					runtime.stateMu.Lock()
					if runtime.pending == pending {
						runtime.pending = nil
					}
					runtime.stateMu.Unlock()
					return productruntime.NativeTurnRef{}, err
				}
				reconcileCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				reconciled, reconcileErr := runtime.reconcilePendingDispatch(reconcileCtx, pending)
				cancel()
				if reconcileErr != nil {
					return productruntime.NativeTurnRef{}, errors.Join(productruntime.ErrAmbiguousSession, err, reconcileErr)
				}
				job, err = reconciled, nil
			}
		}
	} else {
		baseline = previous.UpdatedAt
		reply, replyErr := runtime.client.ReplyJob(ctx, previous.ID, prompt)
		if replyErr != nil {
			return productruntime.NativeTurnRef{}, replyErr
		}
		if reply.Saved {
			job, err = runtime.client.RespawnJob(ctx, previous.ID, reference.NativeSessionID, runtime.cwd)
		} else {
			job, err = runtime.client.GetJob(ctx, previous.ID, reference.NativeSessionID, runtime.cwd)
		}
	}
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if err := runtime.validateJob(job, func() string {
		if previous != nil {
			return previous.ID
		}
		return ""
	}()); err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if firstBinding {
		if pending == nil || job.Name != pending.marker {
			return productruntime.NativeTurnRef{}, fmt.Errorf("%w: first CodeBuddy dispatch changed its exact marker", productruntime.ErrAmbiguousSession)
		}
		if _, existed := pending.baseline[job.ID]; existed {
			return productruntime.NativeTurnRef{}, fmt.Errorf("%w: first CodeBuddy dispatch reused a pre-existing job identity", productruntime.ErrAmbiguousSession)
		}
	}
	runtime.stateMu.Lock()
	defer runtime.stateMu.Unlock()
	if err := runtime.usableLocked(reference); err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	boundReference := reference
	if boundReference.NativeSessionID == "" {
		boundReference.NativeSessionID = job.SessionID
		runtime.ref = boundReference
	}
	runtime.pending = nil
	runtime.job = &job
	runtime.nextTurn++
	turnID := job.ID
	if !firstBinding {
		turnID = turnHandle(boundReference, request.ReceiptID, runtime.nextTurn)
	}
	runtime.active[turnID] = turnObservation{jobID: job.ID, baseline: baseline, seenRunning: job.State == "working" || job.State == "blocked"}
	return productruntime.NativeTurnRef{NativeSessionRef: boundReference, NativeTurnID: turnID}, nil
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, turn productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	runtime, err := driver.runtime(turn.NativeSessionRef)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	runtime.waitMu.Lock()
	defer runtime.waitMu.Unlock()
	if terminal, ok := runtime.cachedTerminal(turn); ok {
		return terminal, nil
	}
	observation, err := runtime.observation(turn)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	streamWake := make(chan struct{}, 1)
	streamDone := make(chan struct{})
	defer func() {
		cancelStream()
		<-streamDone
	}()
	go func() {
		defer close(streamDone)
		_ = runtime.client.StreamJob(streamCtx, observation.jobID, func(event productserver.Event) error {
			select {
			case streamWake <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	ticker := time.NewTicker(driver.config.PollInterval)
	defer ticker.Stop()
	for {
		runtime.nativeMu.Lock()
		if _, err := runtime.observation(turn); err != nil {
			runtime.nativeMu.Unlock()
			return productruntime.NativeTerminal{}, err
		}
		job, getErr := runtime.client.GetJob(ctx, observation.jobID, runtime.ref.NativeSessionID, runtime.cwd)
		if getErr != nil {
			runtime.nativeMu.Unlock()
			return productruntime.NativeTerminal{}, getErr
		}
		if err := runtime.validateJob(job, observation.jobID); err != nil {
			runtime.nativeMu.Unlock()
			return productruntime.NativeTerminal{}, err
		}
		if job.State == "working" || job.State == "blocked" {
			observation.seenRunning = true
		}
		if terminal, terminalJob := terminalFromJob(job); terminalJob &&
			(observation.seenRunning || observation.baseline == 0 || job.UpdatedAt > observation.baseline || job.FirstTerminalAt > observation.baseline) {
			runtime.stateMu.Lock()
			current, stillActive := runtime.active[turn.NativeTurnID]
			if runtime.usableLocked(turn.NativeSessionRef) == nil && stillActive && current.jobID == observation.jobID {
				delete(runtime.active, turn.NativeTurnID)
				runtime.job = &job
				runtime.lastTurnID = turn.NativeTurnID
				runtime.lastTerminal = terminal
				runtime.stateMu.Unlock()
				runtime.nativeMu.Unlock()
				return terminal, nil
			}
			if runtime.ref == turn.NativeSessionRef && runtime.lastTurnID == turn.NativeTurnID {
				terminal = runtime.lastTerminal
				runtime.stateMu.Unlock()
				runtime.nativeMu.Unlock()
				return terminal, nil
			}
			runtime.stateMu.Unlock()
			runtime.nativeMu.Unlock()
			return productruntime.NativeTerminal{}, productruntime.ErrStale
		}
		runtime.nativeMu.Unlock()
		select {
		case <-streamWake:
		case <-ticker.C:
		case <-ctx.Done():
			return productruntime.NativeTerminal{}, errors.Join(productruntime.ErrTimedOut, ctx.Err())
		}
	}
}

func (driver *LaneDriver) Steer(ctx context.Context, turn productruntime.NativeTurnRef, request productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	runtime, err := driver.runtime(turn.NativeSessionRef)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	runtime.nativeMu.Lock()
	defer runtime.nativeMu.Unlock()
	runtime.stateMu.Lock()
	stateErr := runtime.usableLocked(turn.NativeSessionRef)
	observation, ok := runtime.active[turn.NativeTurnID]
	jobMatches := runtime.job != nil && runtime.job.ID == observation.jobID
	runtime.stateMu.Unlock()
	if stateErr != nil {
		return productruntime.NativeAcceptance{}, stateErr
	}
	if !ok || !jobMatches {
		return productruntime.NativeAcceptance{}, productruntime.ErrStale
	}
	permission, err := MapLanePermission(request.PermissionMode, driver.config.AllowSandboxBypass)
	if err != nil || permission.Mode != runtime.permission.Mode {
		return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedPolicy
	}
	prompt, err := readReceipt(driver.deps.Receipts, request.ReceiptID)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	reply, err := runtime.client.ReplyJob(ctx, observation.jobID, prompt)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if reply.Saved {
		job, respawnErr := runtime.client.RespawnJob(ctx, observation.jobID, turn.NativeSessionID, runtime.cwd)
		if respawnErr != nil {
			return productruntime.NativeAcceptance{}, errors.Join(productruntime.ErrAmbiguousSession, respawnErr)
		}
		if validateErr := runtime.validateJob(job, observation.jobID); validateErr != nil {
			return productruntime.NativeAcceptance{}, errors.Join(productruntime.ErrAmbiguousSession, validateErr)
		}
		runtime.stateMu.Lock()
		current, stillActive := runtime.active[turn.NativeTurnID]
		stillExact := runtime.usableLocked(turn.NativeSessionRef) == nil && stillActive && current.jobID == observation.jobID &&
			runtime.job != nil && runtime.job.ID == observation.jobID
		if stillExact {
			runtime.job = &job
		}
		runtime.stateMu.Unlock()
		if !stillExact {
			return productruntime.NativeAcceptance{}, fmt.Errorf("%w: saved CodeBuddy reply respawn raced native state", productruntime.ErrAmbiguousSession)
		}
	}
	return productruntime.NativeAcceptance{
		NativeSessionID: turn.NativeSessionID, AcceptedAt: driver.config.Now().UTC(),
	}, nil
}

func (driver *LaneDriver) Interrupt(ctx context.Context, turn productruntime.NativeTurnRef) error {
	runtime, err := driver.runtime(turn.NativeSessionRef)
	if err != nil {
		return err
	}
	runtime.nativeMu.Lock()
	defer runtime.nativeMu.Unlock()
	runtime.stateMu.Lock()
	stateErr := runtime.usableLocked(turn.NativeSessionRef)
	observation, active := runtime.active[turn.NativeTurnID]
	runtime.stateMu.Unlock()
	if stateErr != nil {
		return stateErr
	}
	if !active {
		return productruntime.ErrStale
	}
	return runtime.client.StopJob(ctx, observation.jobID)
}

func (driver *LaneDriver) Archive(ctx context.Context, reference productruntime.NativeSessionRef) error {
	driver.mu.Lock()
	runtime := driver.lanes[reference.LaneID]
	driver.mu.Unlock()
	if runtime == nil {
		return productruntime.ErrStale
	}
	runtime.nativeMu.Lock()
	defer runtime.nativeMu.Unlock()
	runtime.stateMu.Lock()
	if runtime.ref != reference {
		runtime.stateMu.Unlock()
		return productruntime.ErrStale
	}
	if runtime.archived {
		runtime.stateMu.Unlock()
		return nil
	}
	if runtime.archiving {
		runtime.stateMu.Unlock()
		return productruntime.ErrStale
	}
	if len(runtime.active) != 0 {
		runtime.stateMu.Unlock()
		return fmt.Errorf("%w: cannot archive an active CodeBuddy turn", productruntime.ErrNativeRejected)
	}
	runtime.archiving = true
	jobID := ""
	if runtime.job != nil {
		jobID = runtime.job.ID
	}
	pending := runtime.pending
	runtime.stateMu.Unlock()
	recordDebt := func() {
		runtime.stateMu.Lock()
		if !runtime.archived {
			runtime.archiving = false
			runtime.cleanupDebt = true
		}
		runtime.stateMu.Unlock()
	}
	if jobID == "" && pending != nil {
		job, err := runtime.reconcilePendingDispatch(ctx, pending)
		if err != nil {
			recordDebt()
			return errors.Join(productruntime.ErrCleanupDebt, err)
		}
		jobID = job.ID
		runtime.stateMu.Lock()
		if runtime.pending == pending {
			runtime.job = &job
			runtime.pending = nil
		}
		runtime.stateMu.Unlock()
	}
	if jobID != "" {
		result, err := runtime.client.DeleteJob(ctx, jobID)
		if err != nil {
			recordDebt()
			return errors.Join(productruntime.ErrCleanupDebt, err)
		}
		if !result.Deleted {
			recordDebt()
			return fmt.Errorf("%w: guarded job archive was refused", productruntime.ErrCleanupDebt)
		}
		// The deletion acknowledgement is the commit point for the native job.
		// Clear it before stopping the server so a failed Close retries only the
		// still-outstanding server absence proof, never an already-deleted job.
		runtime.stateMu.Lock()
		if runtime.job != nil && runtime.job.ID == jobID {
			runtime.job = nil
		}
		runtime.stateMu.Unlock()
	}
	if err := runtime.server.Close(ctx); err != nil {
		recordDebt()
		return errors.Join(productruntime.ErrCleanupDebt, err)
	}
	runtime.stateMu.Lock()
	runtime.archived = true
	runtime.archiving = false
	runtime.cleanupDebt = false
	runtime.active = nil
	runtime.stateMu.Unlock()
	return nil
}

func (driver *LaneDriver) Recover(ctx context.Context, request productruntime.LaneRecoveryRequest) (productruntime.NativeSessionRef, error) {
	if request.ProductID != ProductID || !validNativeID(request.LaneID) || !validNativeID(request.PriorNativeSessionID) || request.PriorGeneration == 0 {
		return productruntime.NativeSessionRef{}, productruntime.ErrUnsupportedRecovery
	}
	driver.mu.Lock()
	runtime := driver.lanes[request.LaneID]
	driver.mu.Unlock()
	if runtime != nil {
		runtime.nativeMu.Lock()
		runtime.stateMu.Lock()
		ref := runtime.ref
		stateErr := runtime.usableLocked(ref)
		jobID := ""
		if runtime.job != nil {
			jobID = runtime.job.ID
		}
		runtime.stateMu.Unlock()
		if stateErr == nil {
			if ref.NativeSessionID != request.PriorNativeSessionID {
				runtime.nativeMu.Unlock()
				return productruntime.NativeSessionRef{}, productruntime.ErrAmbiguousSession
			}
			if jobID == "" {
				runtime.nativeMu.Unlock()
				return productruntime.NativeSessionRef{}, productruntime.ErrUnsupportedRecovery
			}
			job, getErr := runtime.client.GetJob(ctx, jobID, ref.NativeSessionID, runtime.cwd)
			if getErr == nil {
				getErr = runtime.validateJob(job, jobID)
			}
			if getErr != nil {
				runtime.nativeMu.Unlock()
				return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrUnsupportedRecovery, getErr)
			}
			runtime.stateMu.Lock()
			stillExact := runtime.usableLocked(ref) == nil && runtime.job != nil && runtime.job.ID == jobID
			if stillExact {
				runtime.job = &job
			}
			runtime.stateMu.Unlock()
			runtime.nativeMu.Unlock()
			if !stillExact {
				return productruntime.NativeSessionRef{}, productruntime.ErrStale
			}
			return ref, nil
		}
		runtime.nativeMu.Unlock()
		if errors.Is(stateErr, productruntime.ErrCleanupDebt) {
			return productruntime.NativeSessionRef{}, stateErr
		}
	}
	openRequest, err := driver.config.Recovery.LaneOpenRequest(ctx, request)
	if err != nil {
		return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrUnsupportedRecovery, err)
	}
	if openRequest.ResumeNativeID != request.PriorNativeSessionID || openRequest.LaneID != request.LaneID || openRequest.ProductID != ProductID {
		return productruntime.NativeSessionRef{}, productruntime.ErrAmbiguousSession
	}
	if err := driver.validateOpen(openRequest); err != nil {
		return productruntime.NativeSessionRef{}, errors.Join(productruntime.ErrUnsupportedRecovery, err)
	}
	return driver.open(ctx, openRequest)
}

func (driver *LaneDriver) validateOpen(request productruntime.LaneOpenRequest) error {
	if driver == nil || driver.deps.OwnedProcesses == nil || driver.deps.Receipts == nil || driver.deps.Generation == 0 ||
		request.ProductID != ProductID || !validNativeID(request.LaneID) || !filepath.IsAbs(request.Cwd) ||
		request.ResumeNativeID != "" && !validNativeID(request.ResumeNativeID) {
		return ErrInvalidConfiguration
	}
	if len(request.Arguments) != 0 || strings.TrimSpace(request.ProfileIdentity) != "" {
		return productruntime.ErrUnsupportedPolicy
	}
	_, err := MapLanePermission(request.PermissionMode, driver.config.AllowSandboxBypass)
	return err
}

func (driver *LaneDriver) laneCommand(request productruntime.LaneOpenRequest, nativeID, port string, password productruntime.SensitiveValue, permission NativePermission) (productruntime.NativeCommand, error) {
	if password.Empty() {
		return productruntime.NativeCommand{}, ErrInvalidConfiguration
	}
	environment := []productruntime.EnvVar{
		{Name: GatewayAuthEnv, Value: "password"},
		{Name: ProductEnv, Value: ProductID},
	}
	environment = append(environment, permission.Env...)
	arguments := []string{"--serve", "--auth", "password", "--port", port,
		"--strict-mcp-config", "--mcp-config", driver.config.MCPConfigPath}
	if nativeID != "" {
		environment = append(environment, productruntime.EnvVar{Name: SessionIDEnv, Value: nativeID})
		arguments = append(arguments, "--session-id", nativeID)
	}
	return productruntime.NativeCommand{
		Path:         driver.config.Executable,
		Args:         arguments,
		Env:          environment,
		SensitiveEnv: []productruntime.SensitiveEnvVar{{Name: GatewayPasswordEnv, Value: password}},
		Cwd:          request.Cwd,
	}, nil
}

func (driver *LaneDriver) runtime(reference productruntime.NativeSessionRef) (*laneRuntime, error) {
	if driver == nil || !validNativeID(reference.LaneID) || reference.NativeSessionID != "" && !validNativeID(reference.NativeSessionID) || reference.Generation == 0 {
		return nil, productruntime.ErrStale
	}
	driver.mu.Lock()
	runtime := driver.lanes[reference.LaneID]
	driver.mu.Unlock()
	if runtime == nil {
		return nil, productruntime.ErrStale
	}
	runtime.stateMu.Lock()
	err := runtime.usableLocked(reference)
	runtime.stateMu.Unlock()
	if err != nil {
		return nil, err
	}
	return runtime, nil
}

func (runtime *laneRuntime) isArchived() bool {
	runtime.stateMu.Lock()
	defer runtime.stateMu.Unlock()
	return runtime.archived
}

func (runtime *laneRuntime) usableLocked(reference productruntime.NativeSessionRef) error {
	if runtime.ref != reference || runtime.archived || runtime.archiving {
		return productruntime.ErrStale
	}
	if runtime.cleanupDebt {
		return productruntime.ErrCleanupDebt
	}
	return nil
}

func (runtime *laneRuntime) validateJob(job AgentJob, expectedJobID string) error {
	if !job.valid() || expectedJobID != "" && job.ID != expectedJobID ||
		runtime.ref.NativeSessionID != "" && job.SessionID != runtime.ref.NativeSessionID || filepath.Clean(job.Cwd) != runtime.cwd {
		return fmt.Errorf("%w: CodeBuddy job changed lane session or cwd identity", productruntime.ErrProtocol)
	}
	return nil
}

func (runtime *laneRuntime) reconcilePendingDispatch(ctx context.Context, pending *pendingDispatch) (AgentJob, error) {
	if pending == nil || pending.marker == "" || pending.receiptID == "" {
		return AgentJob{}, productruntime.ErrAmbiguousSession
	}
	jobs, err := runtime.client.ListJobs(ctx, runtime.cwd)
	if err != nil {
		return AgentJob{}, errors.Join(productruntime.ErrAmbiguousSession, err)
	}
	candidates := make([]AgentJob, 0, 1)
	for _, job := range jobs {
		if _, existed := pending.baseline[job.ID]; existed || job.Name != pending.marker {
			continue
		}
		if !job.valid() || filepath.Clean(job.Cwd) != runtime.cwd {
			return AgentJob{}, fmt.Errorf("%w: reconciled CodeBuddy job changed identity", productruntime.ErrAmbiguousSession)
		}
		candidates = append(candidates, job)
	}
	if len(candidates) != 1 {
		return AgentJob{}, fmt.Errorf("%w: CodeBuddy dispatch reconciliation found %d exact candidates", productruntime.ErrAmbiguousSession, len(candidates))
	}
	return candidates[0], nil
}

func jobIdentitySet(jobs []AgentJob) map[string]struct{} {
	result := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		result[job.ID] = struct{}{}
	}
	return result
}

func dispatchMarker(reference productruntime.NativeSessionRef, receiptID string) string {
	hash := sha256.Sum256([]byte(reference.LaneID + "\x00" + strconv.FormatUint(reference.Generation, 10) + "\x00" + receiptID))
	return "agent-sessions-" + hex.EncodeToString(hash[:16])
}

func (runtime *laneRuntime) observation(turn productruntime.NativeTurnRef) (turnObservation, error) {
	runtime.stateMu.Lock()
	defer runtime.stateMu.Unlock()
	if err := runtime.usableLocked(turn.NativeSessionRef); err != nil {
		return turnObservation{}, err
	}
	observation, ok := runtime.active[turn.NativeTurnID]
	if !ok || !validNativeID(observation.jobID) {
		return turnObservation{}, productruntime.ErrStale
	}
	return observation, nil
}

func (runtime *laneRuntime) cachedTerminal(turn productruntime.NativeTurnRef) (productruntime.NativeTerminal, bool) {
	runtime.stateMu.Lock()
	defer runtime.stateMu.Unlock()
	if runtime.ref == turn.NativeSessionRef && runtime.lastTurnID == turn.NativeTurnID {
		return runtime.lastTerminal, true
	}
	return productruntime.NativeTerminal{}, false
}

func turnHandle(reference productruntime.NativeSessionRef, receiptID string, sequence uint64) string {
	hash := sha256.Sum256([]byte(reference.LaneID + "\x00" + reference.NativeSessionID + "\x00" +
		strconv.FormatUint(reference.Generation, 10) + "\x00" + receiptID + "\x00" + strconv.FormatUint(sequence, 10)))
	return "turn-" + strconv.FormatUint(sequence, 10) + "-" + hex.EncodeToString(hash[:12])
}

func readReceipt(reader productruntime.ReceiptReader, receiptID string) (string, error) {
	if reader == nil || strings.TrimSpace(receiptID) == "" {
		return "", productruntime.ErrProtocol
	}
	input, length, expectedDigest, err := reader.OpenReceipt(receiptID)
	if err != nil {
		return "", errors.Join(productruntime.ErrUnavailable, err)
	}
	if input == nil || length <= 0 || length > maxNativeTextBytes {
		if input != nil {
			_ = input.Close()
		}
		return "", productruntime.ErrProtocol
	}
	body, readErr := io.ReadAll(io.LimitReader(input, length+1))
	closeErr := input.Close()
	if readErr != nil || closeErr != nil || int64(len(body)) != length || sha256.Sum256(body) != expectedDigest || !utf8.Valid(body) || strings.IndexByte(string(body), 0) >= 0 {
		return "", errors.Join(productruntime.ErrProtocol, readErr, closeErr)
	}
	return string(body), nil
}

var _ productruntime.LaneDriver = (*LaneDriver)(nil)
