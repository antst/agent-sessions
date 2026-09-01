package dsh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const (
	maxPromptBytes        = 1 << 20
	maxResultBytes        = 1 << 20
	maxSessionPages       = 32
	maxSessionInfos       = 1024
	maxTurnMessages       = 1024
	maxSettledTurns       = 256
	maxSettledTurnBytes   = 4 << 20
	maxNativeIDBytes      = 1024
	maxLaneIDBytes        = 1024
	maxProfileBytes       = 256
	maxCwdBytes           = 32 << 10
	maxLaneArguments      = 256
	maxLaneArgumentBytes  = 32 << 10
	maxLaneArgumentsBytes = 1 << 20
	maxArchiveEvidence    = 1024
	processCleanupTimeout = 5 * time.Second
	turnReservedID        = "\x00reserved"
	lanePolicyEnv         = "AGENT_SESSIONS_DSH_LANE_POLICY"
)

type RecoveryResolver func(context.Context, productruntime.LaneRecoveryRequest) (productruntime.LaneOpenRequest, error)

type LaneConfig struct {
	Executable      string
	ACPProfile      string
	DSHHome         string
	ProfileManifest string
	Generation      uint64
	TupleVerifier   TupleVerifier
	Processes       ACPProcessFactory
	Leases          LeaseAuthority
	Receipts        productruntime.ReceiptReader
	Environment     []productruntime.EnvVar
	ResolveRecovery RecoveryResolver
}

type laneSession struct {
	reference      productruntime.NativeSessionRef
	profile        string
	cwd            string
	policy         NativePolicy
	process        ACPProcess
	client         *ACPClient
	cancel         context.CancelFunc
	active         string
	turns          map[string]*laneTurn
	turnOrder      []string
	terminalBytes  int
	poisoned       error
	archiving      bool
	closeAttempted bool
	closed         bool
	cleaned        bool
	released       bool
}

type laneTurn struct {
	reference     productruntime.NativeTurnRef
	future        rpcFuture
	result        []byte
	lastResult    []byte
	resultErr     error
	messageID     string
	messageIDs    map[string]struct{}
	admitted      chan struct{}
	admitOnce     sync.Once
	terminal      *productruntime.NativeTerminal
	terminalErr   error
	evidenceBytes int
	settled       chan struct{}
	settleOnce    sync.Once
}

type lanePermissionPolicy struct {
	mu        sync.Mutex
	sessionID string
	policy    NativePolicy
}

type LaneDriver struct {
	config          LaneConfig
	mu              sync.Mutex
	sessions        map[string]*laneSession
	opening         map[string]struct{}
	archived        map[productruntime.NativeSessionRef]uint64
	archiveOrder    []archiveEvidence
	archiveSequence uint64
}

type archiveEvidence struct {
	reference productruntime.NativeSessionRef
	sequence  uint64
}

func NewLaneDriver(config LaneConfig) (*LaneDriver, error) {
	if config.Executable == "" {
		config.Executable = "dsh"
	}
	if config.ACPProfile == "" {
		config.ACPProfile = "acp"
	}
	if filepath.Base(config.Executable) != "dsh" || len(config.ACPProfile) == 0 || len(config.ACPProfile) > maxProfileBytes || config.ACPProfile != strings.TrimSpace(config.ACPProfile) || strings.ContainsAny(config.ACPProfile, "\x00/\\") || config.Generation == 0 || config.TupleVerifier == nil ||
		config.Processes == nil || config.Leases == nil || config.Receipts == nil {
		return nil, errors.New("DSH lane driver requires profile, generation, tuple, process, lease, and receipt dependencies")
	}
	if err := validateManagedProfile(config.DSHHome, config.ACPProfile, config.ProfileManifest); err != nil {
		return nil, err
	}
	return &LaneDriver{config: config, sessions: make(map[string]*laneSession), opening: make(map[string]struct{}), archived: make(map[productruntime.NativeSessionRef]uint64)}, nil
}

func (*LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	// Cordis peers can steer. ACP lanes cannot: -32602 is shared-ledger queue
	// fallback, so advertising native lane steering would be untruthful.
	return productruntime.LaneCapabilitySet{Steer: false, DurableResume: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	request.PermissionMode = normalizePermission(request.PermissionMode)
	if err := validateOpenRequest(request); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if err := driver.reserveLane(request.LaneID); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	defer driver.unreserveLane(request.LaneID)
	if request.ProfileIdentity != driver.config.ACPProfile {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH lane profile identity does not match the configured ACP profile", productruntime.ErrIncompatible)
	}
	if err := validateManagedProfile(driver.config.DSHHome, driver.config.ACPProfile, driver.config.ProfileManifest); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if _, err := verifyPinnedTuple(ctx, driver.config.TupleVerifier, request.ProfileIdentity); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	policy, err := MapPermission(request.PermissionMode)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	process, client, cancel, permissions, err := driver.startClient(request.Cwd, request.Arguments, policy)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	fail := func(failure error, claim *LeaseClaim) (productruntime.NativeSessionRef, error) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		cleanupErr := process.Cleanup(cleanupCtx)
		cleanupCancel()
		cancel()
		if claim != nil {
			cleanupErr = errors.Join(cleanupErr, driver.releaseLeaseBounded(*claim))
		}
		return productruntime.NativeSessionRef{}, errors.Join(failure, cleanupErr)
	}
	if err := client.Initialize(ctx); err != nil {
		return fail(err, nil)
	}
	listed, err := listACPSessions(ctx, client, request.Cwd)
	if err != nil {
		return fail(err, nil)
	}

	var nativeID string
	var claim *LeaseClaim
	if request.ResumeNativeID != "" {
		nativeID = request.ResumeNativeID
		if !listedSessionAtCwd(listed, nativeID, request.Cwd) {
			return fail(fmt.Errorf("%w: DSH resume identity is not listed at the exact cwd", productruntime.ErrStale), nil)
		}
		candidate := driver.claim(request.LaneID, request.ProfileIdentity, nativeID, process.Ref())
		if err := driver.config.Leases.Acquire(ctx, candidate); err != nil {
			return fail(err, nil)
		}
		claim = &candidate
		var resumed struct {
			SessionID string `json:"sessionId"`
		}
		if err := client.Request(ctx, "session/resume", sessionParams(nativeID, request.Cwd, policy), &resumed); err != nil {
			return fail(err, claim)
		}
		if resumed.SessionID != "" && !validNativeID(resumed.SessionID) {
			return fail(fmt.Errorf("%w: DSH resume returned a non-canonical native identity", productruntime.ErrProtocol), claim)
		}
		if resumed.SessionID != "" && resumed.SessionID != nativeID {
			return fail(fmt.Errorf("%w: DSH resumed a different native session", productruntime.ErrAmbiguousSession), claim)
		}
	} else {
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := client.Request(ctx, "session/new", sessionParams("", request.Cwd, policy), &created); err != nil {
			return fail(err, nil)
		}
		if !validNativeID(created.SessionID) {
			return fail(fmt.Errorf("%w: DSH ACP returned no session identity", productruntime.ErrProtocol), nil)
		}
		nativeID = created.SessionID
		if listedSessionID(listed, nativeID) {
			return fail(fmt.Errorf("%w: DSH session/new reused an existing native identity", productruntime.ErrAmbiguousSession), nil)
		}
		candidate := driver.claim(request.LaneID, request.ProfileIdentity, nativeID, process.Ref())
		if err := driver.config.Leases.Acquire(ctx, candidate); err != nil {
			// The conflicting lease may own this exact native ID. Never issue a
			// close until this process has established exclusive ownership.
			return fail(err, nil)
		}
		claim = &candidate
	}
	if err := permissions.bind(nativeID); err != nil {
		return fail(err, claim)
	}

	reference := productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: nativeID, Generation: driver.config.Generation}
	session := &laneSession{
		reference: reference, profile: request.ProfileIdentity, cwd: request.Cwd, policy: policy,
		process: process, client: client, cancel: cancel, turns: make(map[string]*laneTurn),
	}
	driver.mu.Lock()
	if _, exists := driver.sessions[request.LaneID]; exists {
		driver.mu.Unlock()
		return fail(fmt.Errorf("%w: DSH lane already has a live ACP owner", ErrLeaseConflict), claim)
	}
	delete(driver.archived, reference)
	driver.sessions[request.LaneID] = session
	driver.mu.Unlock()
	return reference, nil
}

func (driver *LaneDriver) StartTurn(ctx context.Context, reference productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	request.PermissionMode = normalizePermission(request.PermissionMode)
	session, err := driver.exactSession(reference)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	policy, err := MapPermission(request.PermissionMode)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if policy != session.policy {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: DSH turn policy differs from its exact session policy", productruntime.ErrUnsupportedPolicy)
	}
	body, err := readReceipt(driver.config.Receipts, request.ReceiptID)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	driver.mu.Lock()
	if session.poisoned != nil {
		failure := session.poisoned
		driver.mu.Unlock()
		return productruntime.NativeTurnRef{}, failure
	}
	if session.active != "" {
		driver.mu.Unlock()
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: DSH ACP prompt is already in flight", productruntime.ErrUnsupportedSteer)
	}
	// Reserve the session before allocating/writing the ACP request. Without
	// this fence two concurrent callers can both observe idle and create native
	// prompts before either callback publishes its request ID.
	session.active = turnReservedID
	driver.mu.Unlock()

	var created *laneTurn
	future, err := session.client.startRequest(ctx, "session/prompt", map[string]any{
		"sessionId": reference.NativeSessionID,
		"prompt":    []map[string]string{{"type": "text", "text": string(body)}},
	}, func(future rpcFuture) {
		turnReference := productruntime.NativeTurnRef{NativeSessionRef: reference, NativeTurnID: future.id}
		created = &laneTurn{reference: turnReference, future: future, admitted: make(chan struct{}), settled: make(chan struct{})}
		driver.mu.Lock()
		if session.active == turnReservedID {
			session.turns[future.id], session.active = created, future.id
		}
		driver.mu.Unlock()
	})
	if err != nil {
		var possibleWrite *possibleACPWriteError
		if errors.As(err, &possibleWrite) {
			failure := fmt.Errorf("%w: DSH ACP prompt write completion is ambiguous: %v", productruntime.ErrAmbiguousSession, possibleWrite)
			return productruntime.NativeTurnRef{}, driver.poisonAfterPossibleWrite(session, failure)
		}
		driver.clearTurnOrReservation(session, created)
		return productruntime.NativeTurnRef{}, err
	}
	created.future = future
	go driver.settleTurn(session, created)
	select {
	case <-created.settled:
		driver.mu.Lock()
		terminalErr := created.terminalErr
		driver.mu.Unlock()
		if terminalErr != nil {
			driver.clearTurn(session, created)
			return productruntime.NativeTurnRef{}, terminalErr
		}
		return created.reference, nil
	case <-created.admitted:
		return created.reference, nil
	case <-ctx.Done():
		failure := fmt.Errorf("%w: DSH ACP prompt may have been written before admission timed out: %v", productruntime.ErrAmbiguousSession, ctx.Err())
		return productruntime.NativeTurnRef{}, driver.poisonAfterPossibleWrite(session, failure)
	}
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, reference productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	session, err := driver.exactSession(reference.NativeSessionRef)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	driver.mu.Lock()
	turn := session.turns[reference.NativeTurnID]
	if turn == nil || turn.reference != reference {
		driver.mu.Unlock()
		return productruntime.NativeTerminal{}, fmt.Errorf("%w: DSH native turn is unknown", productruntime.ErrStale)
	}
	if turn.terminal != nil {
		terminal := *turn.terminal
		driver.mu.Unlock()
		return terminal, nil
	}
	if turn.terminalErr != nil {
		failure := turn.terminalErr
		driver.mu.Unlock()
		return productruntime.NativeTerminal{}, failure
	}
	settled := turn.settled
	driver.mu.Unlock()
	select {
	case <-ctx.Done():
		return productruntime.NativeTerminal{}, fmt.Errorf("%w: DSH ACP wait: %v", productruntime.ErrTimedOut, ctx.Err())
	case <-settled:
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if turn.terminalErr != nil {
		return productruntime.NativeTerminal{}, turn.terminalErr
	}
	if turn.terminal == nil {
		return productruntime.NativeTerminal{}, fmt.Errorf("%w: DSH ACP turn settled without a terminal", productruntime.ErrProtocol)
	}
	return *turn.terminal, nil
}

func (*LaneDriver) Steer(context.Context, productruntime.NativeTurnRef, productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedSteer
}

func (driver *LaneDriver) Interrupt(ctx context.Context, reference productruntime.NativeTurnRef) error {
	session, err := driver.exactSession(reference.NativeSessionRef)
	if err != nil {
		return err
	}
	driver.mu.Lock()
	active := session.active == reference.NativeTurnID && session.turns[reference.NativeTurnID] != nil
	driver.mu.Unlock()
	if !active {
		return fmt.Errorf("%w: DSH native turn is not active", productruntime.ErrStale)
	}
	// ACP session/cancel is intentionally a JSON-RPC notification. Adding an id
	// changes it into the unsupported request form (-32601).
	return session.client.Notify(ctx, "session/cancel", map[string]string{"sessionId": reference.NativeSessionID})
}

func (driver *LaneDriver) Archive(ctx context.Context, reference productruntime.NativeSessionRef) error {
	driver.mu.Lock()
	if _, complete := driver.archived[reference]; complete {
		driver.mu.Unlock()
		return nil
	}
	session := driver.sessions[reference.LaneID]
	if session == nil || session.reference != reference || reference.Generation != driver.config.Generation {
		driver.mu.Unlock()
		return fmt.Errorf("%w: DSH native session reference is stale", productruntime.ErrStale)
	}
	if session.active != "" && session.poisoned == nil {
		driver.mu.Unlock()
		return fmt.Errorf("%w: refuse to archive active DSH ACP session", productruntime.ErrNativeRejected)
	}
	if session.archiving {
		driver.mu.Unlock()
		return fmt.Errorf("%w: DSH archive is already reconciling", productruntime.ErrCleanupDebt)
	}
	session.archiving = true
	driver.mu.Unlock()
	defer func() {
		driver.mu.Lock()
		session.archiving = false
		driver.mu.Unlock()
	}()
	return driver.archiveSteps(ctx, session)
}

func (driver *LaneDriver) Recover(ctx context.Context, request productruntime.LaneRecoveryRequest) (productruntime.NativeSessionRef, error) {
	if request.ProductID != ProductID || request.LaneID == "" || len(request.LaneID) > maxLaneIDBytes || !validNativeID(request.PriorNativeSessionID) || request.PriorGeneration == 0 || request.PriorGeneration >= driver.config.Generation {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH recovery request is invalid", productruntime.ErrUnsupportedRecovery)
	}
	if driver.config.ResolveRecovery == nil {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: DSH recovery metadata resolver is unavailable", productruntime.ErrUnsupportedRecovery)
	}
	if err := driver.reserveLane(request.LaneID); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	defer driver.unreserveLane(request.LaneID)
	open, err := driver.config.ResolveRecovery(ctx, request)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	open.PermissionMode = normalizePermission(open.PermissionMode)
	open.ProductID, open.LaneID, open.ResumeNativeID = ProductID, request.LaneID, request.PriorNativeSessionID
	if err := validateOpenRequest(open); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if open.ProfileIdentity != driver.config.ACPProfile {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: recovered DSH profile identity does not match the configured ACP profile", productruntime.ErrIncompatible)
	}
	if err := validateManagedProfile(driver.config.DSHHome, driver.config.ACPProfile, driver.config.ProfileManifest); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if _, err := verifyPinnedTuple(ctx, driver.config.TupleVerifier, open.ProfileIdentity); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	policy, err := MapPermission(open.PermissionMode)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	process, client, cancel, permissions, err := driver.startClient(open.Cwd, open.Arguments, policy)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	cleanup := func(failure error, release bool) (productruntime.NativeSessionRef, error) {
		cleanupErr := cleanupACPProcess(process)
		cancel()
		if release {
			cleanupErr = errors.Join(cleanupErr, driver.releaseLeaseBounded(driver.claim(open.LaneID, open.ProfileIdentity, open.ResumeNativeID, process.Ref())))
		}
		return productruntime.NativeSessionRef{}, errors.Join(failure, cleanupErr)
	}
	if err := client.Initialize(ctx); err != nil {
		return cleanup(err, false)
	}
	listed, err := listACPSessions(ctx, client, open.Cwd)
	if err != nil {
		return cleanup(err, false)
	}
	if !listedSessionAtCwd(listed, open.ResumeNativeID, open.Cwd) {
		return cleanup(fmt.Errorf("%w: recovered DSH identity is not listed at the exact cwd", productruntime.ErrStale), false)
	}
	claim := driver.claim(open.LaneID, open.ProfileIdentity, open.ResumeNativeID, process.Ref())
	if err := driver.config.Leases.Recover(ctx, claim); err != nil {
		return cleanup(err, false)
	}
	var resumed struct {
		SessionID string `json:"sessionId"`
	}
	if err := client.Request(ctx, "session/resume", sessionParams(open.ResumeNativeID, open.Cwd, policy), &resumed); err != nil {
		return cleanup(err, true)
	}
	if resumed.SessionID != "" && !validNativeID(resumed.SessionID) {
		return cleanup(fmt.Errorf("%w: DSH recovery returned a non-canonical native identity", productruntime.ErrProtocol), true)
	}
	if resumed.SessionID != "" && resumed.SessionID != open.ResumeNativeID {
		return cleanup(fmt.Errorf("%w: DSH recovery resumed a different native session", productruntime.ErrAmbiguousSession), true)
	}
	if err := permissions.bind(open.ResumeNativeID); err != nil {
		return cleanup(err, true)
	}
	reference := productruntime.NativeSessionRef{LaneID: open.LaneID, NativeSessionID: open.ResumeNativeID, Generation: driver.config.Generation}
	session := &laneSession{reference: reference, profile: open.ProfileIdentity, cwd: open.Cwd, policy: policy, process: process, client: client, cancel: cancel, turns: make(map[string]*laneTurn)}
	driver.mu.Lock()
	if _, exists := driver.sessions[open.LaneID]; exists {
		driver.mu.Unlock()
		return cleanup(ErrLeaseConflict, true)
	}
	delete(driver.archived, reference)
	driver.sessions[open.LaneID] = session
	driver.mu.Unlock()
	return reference, nil
}

func (driver *LaneDriver) startClient(cwd string, arguments []string, policy NativePolicy) (ACPProcess, *ACPClient, context.CancelFunc, *lanePermissionPolicy, error) {
	for _, argument := range arguments {
		if overridesDSHProfile(argument) || argument == "--resume" || strings.HasPrefix(argument, "--resume=") {
			return nil, nil, nil, nil, fmt.Errorf("%w: DSH ACP profile/session cannot be overridden", productruntime.ErrUnsupportedPolicy)
		}
	}
	processCtx, cancel := context.WithCancel(context.Background())
	environment := setEnvVar(driver.config.Environment, "DSH_PERMISSION_MODE", string(policy.Sandbox))
	environment = setEnvVar(environment, "DSH_HOME", driver.config.DSHHome)
	environment = setEnvVar(environment, lanePolicyEnv, string(policy.Sandbox)+":"+string(policy.Approval))
	command := productruntime.NativeCommand{
		Path: driver.config.Executable, Args: append([]string{"--profile", driver.config.ACPProfile}, arguments...),
		Env: environment, Cwd: cwd,
	}
	process, err := driver.config.Processes.StartACPProcess(processCtx, command)
	if err != nil {
		cancel()
		return nil, nil, nil, nil, fmt.Errorf("%w: start exact DSH ACP tuple: %v", productruntime.ErrUnavailable, err)
	}
	permissions := &lanePermissionPolicy{policy: policy}
	client, err := NewACPClient(process, driver.handleNotification, permissions.handle)
	if err != nil {
		_ = cleanupACPProcess(process)
		cancel()
		return nil, nil, nil, nil, err
	}
	return process, client, cancel, permissions, nil
}

func (driver *LaneDriver) handleNotification(notification acpNotification) {
	if notification.Method != "session/update" {
		return
	}
	var params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind      string `json:"sessionUpdate"`
			MessageID string `json:"messageId"`
			Content   struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(notification.Params, &params) != nil || params.SessionID == "" {
		return
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	for _, session := range driver.sessions {
		if session.reference.NativeSessionID != params.SessionID || session.active == "" {
			continue
		}
		turn := session.turns[session.active]
		if turn == nil || turn.resultErr != nil {
			return
		}
		if params.Update.Kind != "agent_message_chunk" {
			return
		}
		if params.Update.MessageID == "" || len(params.Update.MessageID) > 1024 || params.Update.Content.Type != "text" {
			turn.resultErr = fmt.Errorf("%w: DSH ACP assistant update omitted exact message identity", productruntime.ErrProtocol)
			return
		}
		if turn.messageID != params.Update.MessageID {
			if turn.messageIDs == nil {
				turn.messageIDs = make(map[string]struct{})
			}
			if _, seen := turn.messageIDs[params.Update.MessageID]; seen {
				turn.resultErr = fmt.Errorf("%w: DSH ACP assistant messages interleaved native identities", productruntime.ErrProtocol)
				return
			}
			if len(turn.messageIDs) >= maxTurnMessages {
				turn.resultErr = fmt.Errorf("%w: DSH ACP turn exceeds bounded assistant-message identities", productruntime.ErrProtocol)
				return
			}
			if len(turn.result) != 0 {
				turn.lastResult = append(turn.lastResult[:0], turn.result...)
			}
			turn.result = turn.result[:0]
			turn.messageIDs[params.Update.MessageID] = struct{}{}
			turn.messageID = params.Update.MessageID
		}
		turn.admitOnce.Do(func() { close(turn.admitted) })
		if len(params.Update.Content.Text) > maxResultBytes-len(turn.result) {
			turn.resultErr = fmt.Errorf("%w: DSH ACP result exceeds fixed bound", productruntime.ErrProtocol)
			return
		}
		turn.result = append(turn.result, params.Update.Content.Text...)
		return
	}
}

func (driver *LaneDriver) exactSession(reference productruntime.NativeSessionRef) (*laneSession, error) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	session := driver.sessions[reference.LaneID]
	if session == nil || session.reference != reference || reference.Generation != driver.config.Generation {
		return nil, fmt.Errorf("%w: DSH native session reference is stale", productruntime.ErrStale)
	}
	return session, nil
}

func (driver *LaneDriver) reserveLane(laneID string) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.sessions[laneID] != nil {
		return fmt.Errorf("%w: DSH lane already has a live ACP owner", ErrLeaseConflict)
	}
	if _, exists := driver.opening[laneID]; exists {
		return fmt.Errorf("%w: DSH lane open is already in progress", ErrLeaseConflict)
	}
	driver.opening[laneID] = struct{}{}
	return nil
}

func (driver *LaneDriver) unreserveLane(laneID string) {
	driver.mu.Lock()
	delete(driver.opening, laneID)
	driver.mu.Unlock()
}

func (driver *LaneDriver) clearTurn(session *laneSession, turn *laneTurn) {
	if turn == nil {
		return
	}
	driver.mu.Lock()
	turnID := turn.reference.NativeTurnID
	if current := session.turns[turnID]; current == turn {
		delete(session.turns, turnID)
		for index, cachedID := range session.turnOrder {
			if cachedID != turnID {
				continue
			}
			session.terminalBytes -= turn.evidenceBytes
			copy(session.turnOrder[index:], session.turnOrder[index+1:])
			session.turnOrder = session.turnOrder[:len(session.turnOrder)-1]
			break
		}
	}
	if session.active == turnID {
		session.active = ""
	}
	driver.mu.Unlock()
}

func (driver *LaneDriver) clearTurnOrReservation(session *laneSession, turn *laneTurn) {
	if turn != nil {
		driver.clearTurn(session, turn)
		return
	}
	driver.mu.Lock()
	if session.active == turnReservedID {
		session.active = ""
	}
	driver.mu.Unlock()
}

func (driver *LaneDriver) clearActive(session *laneSession, turnID string) {
	driver.mu.Lock()
	if session.active == turnID {
		session.active = ""
	}
	driver.mu.Unlock()
}

func (driver *LaneDriver) settleTurn(session *laneSession, turn *laneTurn) {
	response := <-turn.future.done
	var terminal *productruntime.NativeTerminal
	var terminalErr error
	if response.err != nil {
		terminalErr = mapACPError("session/prompt", response.err)
	} else {
		var completed struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(response.result, &completed); err != nil || completed.StopReason == "" {
			terminalErr = fmt.Errorf("%w: DSH ACP prompt omitted stop reason", productruntime.ErrProtocol)
		} else {
			driver.mu.Lock()
			resultBytes, resultErr := turn.result, turn.resultErr
			if len(resultBytes) == 0 && len(turn.lastResult) != 0 {
				resultBytes = turn.lastResult
			}
			result := string(resultBytes)
			driver.mu.Unlock()
			if resultErr != nil {
				terminalErr = resultErr
			} else {
				value := nativeTerminal(completed.StopReason, result)
				terminal = &value
			}
		}
	}
	driver.mu.Lock()
	turn.terminal, turn.terminalErr = terminal, terminalErr
	if terminal != nil {
		turn.evidenceBytes = len(terminal.Result)
	} else if terminalErr != nil {
		turn.evidenceBytes = len(terminalErr.Error())
	}
	turn.result, turn.lastResult, turn.messageIDs, turn.messageID = nil, nil, nil, ""
	if session.active == turn.reference.NativeTurnID {
		session.active = ""
	}
	driver.cacheSettledTurnLocked(session, turn)
	turn.settleOnce.Do(func() { close(turn.settled) })
	driver.mu.Unlock()
}

func (driver *LaneDriver) cacheSettledTurnLocked(session *laneSession, turn *laneTurn) {
	session.turnOrder = append(session.turnOrder, turn.reference.NativeTurnID)
	session.terminalBytes += turn.evidenceBytes
	for len(session.turnOrder) > maxSettledTurns || session.terminalBytes > maxSettledTurnBytes {
		oldestID := session.turnOrder[0]
		session.turnOrder = session.turnOrder[1:]
		oldest := session.turns[oldestID]
		if oldest == nil {
			continue
		}
		session.terminalBytes -= oldest.evidenceBytes
		delete(session.turns, oldestID)
	}
}

func (driver *LaneDriver) poisonAfterPossibleWrite(session *laneSession, failure error) error {
	driver.mu.Lock()
	if session.poisoned == nil {
		session.poisoned = failure
	}
	if session.archiving {
		driver.mu.Unlock()
		return failure
	}
	session.archiving = true
	driver.mu.Unlock()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cleanupErr := driver.archiveSteps(cleanupCtx, session)
	cancel()
	driver.mu.Lock()
	session.archiving = false
	driver.mu.Unlock()
	return errors.Join(failure, cleanupErr)
}

func (driver *LaneDriver) archiveSteps(ctx context.Context, session *laneSession) error {
	driver.mu.Lock()
	poisoned, closeAttempted, cleaned, released := session.poisoned != nil, session.closeAttempted, session.cleaned, session.released
	driver.mu.Unlock()
	var closeErr error
	if !poisoned && !closeAttempted {
		closeErr = session.client.Request(ctx, "session/close", map[string]string{"sessionId": session.reference.NativeSessionID}, nil)
		driver.mu.Lock()
		session.closeAttempted = true
		session.closed = closeErr == nil
		driver.mu.Unlock()
		if closeErr != nil {
			closeErr = fmt.Errorf("%w: DSH session/close may have been written before its result was lost: %v", productruntime.ErrAmbiguousSession, closeErr)
		}
	}
	if !cleaned {
		var err error
		if closeErr != nil {
			err = cleanupACPProcess(session.process)
		} else {
			err = session.process.Cleanup(ctx)
		}
		if err != nil {
			return errors.Join(closeErr, err)
		}
		session.cancel()
		driver.mu.Lock()
		session.cleaned = true
		driver.mu.Unlock()
	}
	if !released {
		claim := driver.claim(session.reference.LaneID, session.profile, session.reference.NativeSessionID, session.process.Ref())
		var err error
		if closeErr != nil {
			err = driver.releaseLeaseBounded(claim)
		} else {
			err = driver.config.Leases.Release(ctx, claim)
		}
		if err != nil {
			return errors.Join(closeErr, err)
		}
		driver.mu.Lock()
		session.released = true
		driver.mu.Unlock()
	}
	driver.mu.Lock()
	if current := driver.sessions[session.reference.LaneID]; current == session {
		delete(driver.sessions, session.reference.LaneID)
		driver.markArchiveCompleteLocked(session.reference)
	}
	driver.mu.Unlock()
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func cleanupACPProcess(process ACPProcess) error {
	ctx, cancel := context.WithTimeout(context.Background(), processCleanupTimeout)
	defer cancel()
	return process.Cleanup(ctx)
}

func (driver *LaneDriver) releaseLeaseBounded(claim LeaseClaim) error {
	ctx, cancel := context.WithTimeout(context.Background(), processCleanupTimeout)
	defer cancel()
	return driver.config.Leases.Release(ctx, claim)
}

func (driver *LaneDriver) markArchiveCompleteLocked(reference productruntime.NativeSessionRef) {
	driver.archiveSequence++
	sequence := driver.archiveSequence
	driver.archived[reference] = sequence
	driver.archiveOrder = append(driver.archiveOrder, archiveEvidence{reference: reference, sequence: sequence})
	for len(driver.archiveOrder) > maxArchiveEvidence {
		oldest := driver.archiveOrder[0]
		driver.archiveOrder = driver.archiveOrder[1:]
		if driver.archived[oldest.reference] == oldest.sequence {
			delete(driver.archived, oldest.reference)
		}
	}
}

func (policy *lanePermissionPolicy) bind(sessionID string) error {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if sessionID == "" || policy.sessionID != "" && policy.sessionID != sessionID {
		return fmt.Errorf("%w: DSH ACP permission policy changed native session", productruntime.ErrAmbiguousSession)
	}
	policy.sessionID = sessionID
	return nil
}

func (policy *lanePermissionPolicy) handle(request acpServerRequest) (any, *rpcError) {
	if request.Method != "session/request_permission" {
		return nil, &rpcError{Code: -32601, Message: "unsupported ACP client method"}
	}
	var params struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string `json:"toolCallId"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if len(request.Params) == 0 || len(request.Params) > maxACPFrameBytes || json.Unmarshal(request.Params, &params) != nil ||
		params.SessionID == "" || params.ToolCall.ToolCallID == "" || len(params.Options) == 0 || len(params.Options) > 64 {
		return nil, &rpcError{Code: -32602, Message: "invalid DSH ACP permission request"}
	}
	policy.mu.Lock()
	sessionID, nativePolicy := policy.sessionID, policy.policy
	policy.mu.Unlock()
	if sessionID == "" || params.SessionID != sessionID {
		return nil, &rpcError{Code: -32602, Message: "permission request changed the exact DSH session"}
	}
	if nativePolicy.Approval != ApprovalAsk && nativePolicy.Approval != ApprovalNever {
		return nil, &rpcError{Code: -32602, Message: "DSH ACP approval policy is invalid"}
	}
	// The lane is non-interactive and has no user-approval relay. Neither the
	// ordinary ask policy nor an unexpected callback under approval=never is
	// authority to grant a tool call. Select only the exact one-call rejection.
	wanted := "reject_once"
	knownKinds := map[string]struct{}{"allow_once": {}, "allow_always": {}, "reject_once": {}, "reject_always": {}}
	seenIDs := make(map[string]struct{}, len(params.Options))
	selected := ""
	for _, option := range params.Options {
		if option.OptionID == "" || len(option.OptionID) > 1024 || len(option.Name) > 4096 || len(option.Kind) > 64 {
			return nil, &rpcError{Code: -32602, Message: "invalid DSH ACP permission option"}
		}
		if _, known := knownKinds[option.Kind]; !known {
			return nil, &rpcError{Code: -32602, Message: "unknown DSH ACP permission option kind"}
		}
		if _, duplicate := seenIDs[option.OptionID]; duplicate {
			return nil, &rpcError{Code: -32602, Message: "duplicate DSH ACP permission option identity"}
		}
		seenIDs[option.OptionID] = struct{}{}
		if option.Kind == wanted {
			if selected != "" {
				return nil, &rpcError{Code: -32602, Message: "ambiguous DSH ACP one-call rejection option"}
			}
			selected = option.OptionID
		}
	}
	if selected != "" {
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": selected}}, nil
	}
	return nil, &rpcError{Code: -32602, Message: "DSH ACP permission options cannot represent the lane policy"}
}

type acpSessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

func listACPSessions(ctx context.Context, client *ACPClient, cwd string) ([]acpSessionInfo, error) {
	var result []acpSessionInfo
	seenSessions := make(map[string]struct{})
	cursor := ""
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxSessionPages; page++ {
		params := map[string]any{"cwd": cwd}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response struct {
			Sessions   json.RawMessage `json:"sessions"`
			NextCursor string          `json:"nextCursor"`
		}
		if err := client.Request(ctx, "session/list", params, &response); err != nil {
			return nil, err
		}
		var sessions []acpSessionInfo
		if len(response.Sessions) == 0 || bytes.Equal(bytes.TrimSpace(response.Sessions), []byte("null")) || json.Unmarshal(response.Sessions, &sessions) != nil {
			return nil, fmt.Errorf("%w: DSH ACP session/list omitted its sessions array", productruntime.ErrProtocol)
		}
		if len(sessions) > maxSessionInfos-len(result) {
			return nil, fmt.Errorf("%w: DSH ACP session/list exceeds fixed bounds", productruntime.ErrProtocol)
		}
		for _, session := range sessions {
			if !validNativeID(session.SessionID) || len(session.Cwd) > maxCwdBytes || !sameCwd(session.Cwd, cwd) {
				return nil, fmt.Errorf("%w: DSH ACP session/list returned an invalid identity or cwd", productruntime.ErrProtocol)
			}
			if _, duplicate := seenSessions[session.SessionID]; duplicate {
				return nil, fmt.Errorf("%w: DSH ACP session/list repeated a native identity", productruntime.ErrProtocol)
			}
			seenSessions[session.SessionID] = struct{}{}
			result = append(result, session)
		}
		if response.NextCursor == "" {
			return result, nil
		}
		if len(response.NextCursor) > 1024 {
			return nil, fmt.Errorf("%w: DSH ACP session/list cursor exceeds fixed bounds", productruntime.ErrProtocol)
		}
		if _, duplicate := seenCursors[response.NextCursor]; duplicate {
			return nil, fmt.Errorf("%w: DSH ACP session/list repeated a cursor", productruntime.ErrProtocol)
		}
		seenCursors[response.NextCursor], cursor = struct{}{}, response.NextCursor
	}
	return nil, fmt.Errorf("%w: DSH ACP session/list pagination exceeds fixed bounds", productruntime.ErrProtocol)
}

func listedSessionAtCwd(sessions []acpSessionInfo, nativeID, cwd string) bool {
	for _, session := range sessions {
		if session.SessionID == nativeID && sameCwd(session.Cwd, cwd) {
			return true
		}
	}
	return false
}

func listedSessionID(sessions []acpSessionInfo, nativeID string) bool {
	for _, session := range sessions {
		if session.SessionID == nativeID {
			return true
		}
	}
	return false
}

func sameCwd(left, right string) bool {
	if !filepath.IsAbs(left) || !filepath.IsAbs(right) || filepath.Clean(left) != left || filepath.Clean(right) != right {
		return false
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		return resolvedLeft == resolvedRight
	}
	return left == right
}

func (driver *LaneDriver) claim(laneID, profile, nativeID string, process productruntime.OwnedProcessRef) LeaseClaim {
	return LeaseClaim{ProfileIdentity: profile, NativeSessionID: nativeID, LaneID: laneID, Generation: driver.config.Generation, Process: process}
}

func sessionParams(nativeID, cwd string, _ NativePolicy) map[string]any {
	parameters := map[string]any{
		"cwd": cwd, "mcpServers": []any{},
	}
	if nativeID != "" {
		parameters["sessionId"] = nativeID
	}
	return parameters
}

func setEnvVar(environment []productruntime.EnvVar, name, value string) []productruntime.EnvVar {
	result := make([]productruntime.EnvVar, 0, len(environment)+1)
	for _, variable := range environment {
		if variable.Name != name {
			result = append(result, variable)
		}
	}
	return append(result, productruntime.EnvVar{Name: name, Value: value})
}

func validateOpenRequest(request productruntime.LaneOpenRequest) error {
	if request.ProductID != ProductID || request.LaneID == "" || len(request.LaneID) > maxLaneIDBytes || request.ResumeNativeID != "" && !validNativeID(request.ResumeNativeID) ||
		!validCwd(request.Cwd) ||
		request.ProfileIdentity == "" || len(request.ProfileIdentity) > maxProfileBytes || !request.PermissionMode.Valid() || len(request.Arguments) > maxLaneArguments {
		return fmt.Errorf("%w: DSH lane open request is incomplete", productruntime.ErrNativeRejected)
	}
	totalArguments := 0
	for _, argument := range request.Arguments {
		if len(argument) > maxLaneArgumentBytes {
			return fmt.Errorf("%w: DSH lane argument exceeds its fixed bound", productruntime.ErrNativeRejected)
		}
		totalArguments += len(argument)
		if totalArguments > maxLaneArgumentsBytes {
			return fmt.Errorf("%w: DSH lane arguments exceed their fixed total bound", productruntime.ErrNativeRejected)
		}
	}
	return nil
}

func validNativeID(nativeID string) bool {
	return nativeID != "" && len(nativeID) <= maxNativeIDBytes && strings.TrimSpace(nativeID) == nativeID && !strings.ContainsAny(nativeID, "\x00\r\n")
}

func validCwd(cwd string) bool {
	return filepath.IsAbs(cwd) && filepath.Clean(cwd) == cwd && len(cwd) <= maxCwdBytes
}

func normalizePermission(mode permissionmode.Mode) permissionmode.Mode {
	if mode == "" {
		return permissionmode.Default
	}
	return mode
}

func readReceipt(reader productruntime.ReceiptReader, receiptID string) ([]byte, error) {
	if reader == nil || receiptID == "" {
		return nil, fmt.Errorf("%w: DSH turn requires durable receipt", productruntime.ErrNativeRejected)
	}
	stream, size, digest, err := reader.OpenReceipt(receiptID)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	if size < 1 || size > maxPromptBytes {
		return nil, fmt.Errorf("%w: DSH receipt size is outside fixed bounds", productruntime.ErrProtocol)
	}
	body, err := io.ReadAll(io.LimitReader(stream, maxPromptBytes+1))
	if err != nil || int64(len(body)) != size || len(body) > maxPromptBytes || sha256.Sum256(body) != digest {
		return nil, fmt.Errorf("%w: DSH receipt identity, size, or digest changed", productruntime.ErrProtocol)
	}
	return body, nil
}

func nativeTerminal(stopReason, result string) productruntime.NativeTerminal {
	terminal := productruntime.NativeTerminal{Result: result, ResultDigest: sha256.Sum256([]byte(result)), NativeStopReason: stopReason}
	switch stopReason {
	case "end_turn", "max_tokens", "max_turn_requests":
		terminal.Outcome = productruntime.TurnCompleted
	case "cancelled":
		terminal.Outcome, terminal.ExitLike = productruntime.TurnInterrupted, 130
	default:
		terminal.Outcome, terminal.ExitLike = productruntime.TurnFailed, 1
	}
	return terminal
}

var _ productruntime.LaneDriver = (*LaneDriver)(nil)
