package opencodefamily

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type PermissionMapper func(permissionmode.Mode) ([]PermissionRule, error)

type LaneConfig struct {
	ProductID        string
	Dialect          Dialect
	Generation       uint64
	Servers          ServerManager
	MapPermission    PermissionMapper
	DecidePermission PermissionDecision
	Now              func() time.Time
}

type laneSession struct {
	cleanup           sync.Mutex
	server            *LiveServer
	client            *Client
	nativeID          string
	permissionMode    permissionmode.Mode
	intentDigest      [32]byte
	provisional       bool
	opening           bool
	openDone          chan struct{}
	fresh             bool
	deleted           bool
	turn              *laneTurn
	interruptedTurnID string
	archiving         bool
	archived          bool
}

type laneTurn struct {
	id          string
	operationID string
	waiting     bool
	waitCancel  context.CancelFunc
}

// LaneDriver retains only live server/client handles and native IDs. Names,
// cwd, endpoints, and credentials are never copied into durable references.
type LaneDriver struct {
	config LaneConfig
	mu     sync.Mutex
	lanes  map[string]*laneSession
}

func NewLaneDriver(config LaneConfig) (*LaneDriver, error) {
	if config.ProductID == "" || config.Generation == 0 || config.Servers == nil ||
		config.MapPermission == nil || config.Dialect != DialectOpenCode && config.Dialect != DialectKilo {
		return nil, productruntime.ErrProtocol
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &LaneDriver{config: config, lanes: make(map[string]*laneSession)}, nil
}

func (driver *LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{Steer: driver.config.Dialect == DialectKilo, DurableResume: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if request.ProductID != driver.config.ProductID || strings.TrimSpace(request.LaneID) == "" || !validDirectory(request.Cwd) ||
		!request.PermissionMode.Valid() || unsafeServerArguments(request.Arguments) {
		return productruntime.NativeSessionRef{}, productruntime.ErrProtocol
	}
	permissions, err := driver.config.MapPermission(request.PermissionMode)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	digest := laneIntentDigest("open", request, request.PermissionMode)
	driver.mu.Lock()
	if existing := driver.lanes[request.LaneID]; existing != nil {
		if !existing.provisional || existing.intentDigest != digest || existing.opening {
			driver.mu.Unlock()
			return productruntime.NativeSessionRef{}, productruntime.ErrAmbiguousSession
		}
		driver.mu.Unlock()
		if cleanupErr := driver.retryProvisionalCleanup(ctx, request.LaneID, existing); cleanupErr != nil {
			return productruntime.NativeSessionRef{}, cleanupErr
		}
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: prior lane-open cleanup converged; retry launch explicitly", productruntime.ErrCleanupDebt)
	}
	live := &laneSession{
		nativeID: request.ResumeNativeID, permissionMode: request.PermissionMode,
		intentDigest: digest, provisional: true, opening: true, openDone: make(chan struct{}), fresh: request.ResumeNativeID == "",
	}
	driver.lanes[request.LaneID] = live
	driver.mu.Unlock()
	server, err := driver.config.Servers.Open(ctx, ServerOpenRequest{
		Key: request.LaneID, Cwd: request.Cwd, Arguments: request.Arguments, PermissionRules: permissions,
	})
	if server == nil {
		if err == nil {
			err = productruntime.ErrUnavailable
		}
		driver.discardEmptyProvisional(request.LaneID, live)
		return productruntime.NativeSessionRef{}, err
	}
	live.server = server
	if err != nil {
		return productruntime.NativeSessionRef{}, driver.failProvisionalOpen(context.Background(), request.LaneID, live, err)
	}
	client := server.Client()
	if client == nil {
		return productruntime.NativeSessionRef{}, driver.failProvisionalOpen(context.Background(), request.LaneID, live, productruntime.ErrUnavailable)
	}
	live.client = client
	var session Session
	if request.ResumeNativeID == "" {
		session, err = client.CreateSession(ctx, "", permissions)
	} else {
		session, err = client.GetSession(ctx, request.ResumeNativeID)
	}
	if err != nil || request.ResumeNativeID != "" && session.ID != request.ResumeNativeID {
		if err == nil {
			err = productruntime.ErrAmbiguousSession
		}
		return productruntime.NativeSessionRef{}, driver.failProvisionalOpen(context.Background(), request.LaneID, live, err)
	}
	live.cleanup.Lock()
	live.nativeID = session.ID
	live.cleanup.Unlock()
	driver.mu.Lock()
	if driver.lanes[request.LaneID] != live || !live.opening {
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, driver.failProvisionalOpen(context.Background(), request.LaneID, live, productruntime.ErrAmbiguousSession)
	}
	live.provisional = false
	live.opening = false
	close(live.openDone)
	driver.mu.Unlock()
	return productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: session.ID, Generation: driver.config.Generation}, nil
}

func laneIntentDigest(kind string, request any, mode permissionmode.Mode) [32]byte {
	encoded, _ := json.Marshal(struct {
		Kind    string              `json:"kind"`
		Mode    permissionmode.Mode `json:"mode"`
		Request any                 `json:"request"`
	}{Kind: kind, Mode: mode, Request: request})
	return sha256.Sum256(encoded)
}

func (driver *LaneDriver) discardEmptyProvisional(laneID string, live *laneSession) {
	driver.mu.Lock()
	if driver.lanes[laneID] == live {
		delete(driver.lanes, laneID)
	}
	if live.opening {
		live.opening = false
		close(live.openDone)
	}
	driver.mu.Unlock()
}

func (driver *LaneDriver) cleanupProvisionalResources(ctx context.Context, live *laneSession) error {
	if live == nil {
		return nil
	}
	live.cleanup.Lock()
	defer live.cleanup.Unlock()
	if live.fresh && live.nativeID != "" && !live.deleted {
		if live.client == nil {
			return fmt.Errorf("%w: fresh lane cleanup client is unavailable", productruntime.ErrCleanupDebt)
		}
		if err := live.client.DeleteSession(ctx, live.nativeID); err != nil && !errors.Is(err, productruntime.ErrStale) {
			return fmt.Errorf("%w: exact fresh lane session delete: %v", productruntime.ErrCleanupDebt, err)
		}
		live.deleted = true
	}
	if live.server != nil {
		if err := live.server.Close(ctx); err != nil {
			return fmt.Errorf("%w: exact lane server close: %v", productruntime.ErrCleanupDebt, err)
		}
	}
	live.client, live.server = nil, nil
	return nil
}

func (driver *LaneDriver) failProvisionalOpen(ctx context.Context, laneID string, live *laneSession, cause error) error {
	cleanupErr := driver.cleanupProvisionalResources(ctx, live)
	driver.mu.Lock()
	if live.opening {
		live.opening = false
		close(live.openDone)
	}
	if cleanupErr == nil && driver.lanes[laneID] == live {
		delete(driver.lanes, laneID)
	}
	driver.mu.Unlock()
	return errors.Join(cause, cleanupErr)
}

func (driver *LaneDriver) retryProvisionalCleanup(ctx context.Context, laneID string, live *laneSession) error {
	if err := driver.cleanupProvisionalResources(ctx, live); err != nil {
		return err
	}
	driver.mu.Lock()
	if driver.lanes[laneID] == live {
		delete(driver.lanes, laneID)
	}
	driver.mu.Unlock()
	return nil
}

func (driver *LaneDriver) StartTurn(ctx context.Context, session productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	if !request.PermissionMode.Valid() || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > maxResultBytes {
		return productruntime.NativeTurnRef{}, productruntime.ErrProtocol
	}
	operationID, err := newOperationID()
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if _, err := driver.config.MapPermission(request.PermissionMode); err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	live, err := driver.lockLive(session, true)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if live.permissionMode != request.PermissionMode {
		driver.mu.Unlock()
		return productruntime.NativeTurnRef{}, productruntime.ErrUnsupportedPolicy
	}
	if live.turn != nil || live.interruptedTurnID != "" {
		driver.mu.Unlock()
		return productruntime.NativeTurnRef{}, productruntime.ErrNativeRejected
	}
	live.turn = &laneTurn{operationID: operationID}
	driver.mu.Unlock()
	body := []byte(request.Prompt)
	var accepted productruntime.NativeAcceptance
	if driver.config.Dialect == DialectKilo {
		accepted, err = live.client.KiloPrompt(ctx, session.NativeSessionID, operationID, body, "queue")
	} else {
		accepted, err = live.client.PromptAsync(ctx, session.NativeSessionID, operationID, body, false)
	}
	if err != nil {
		driver.clearTurn(session.LaneID, operationID)
		return productruntime.NativeTurnRef{}, err
	}
	driver.mu.Lock()
	current := driver.lanes[session.LaneID]
	if current == nil || current != live || current.turn == nil || current.turn.operationID != operationID {
		driver.mu.Unlock()
		return productruntime.NativeTurnRef{}, productruntime.ErrAmbiguousSession
	}
	current.turn.id = accepted.NativeMessageID
	driver.mu.Unlock()
	return productruntime.NativeTurnRef{NativeSessionRef: session, NativeTurnID: accepted.NativeMessageID}, nil
}

func (driver *LaneDriver) Steer(ctx context.Context, turn productruntime.NativeTurnRef, request productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	if driver.config.Dialect != DialectKilo {
		return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedSteer
	}
	if !request.PermissionMode.Valid() || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > maxResultBytes || turn.NativeTurnID == "" {
		return productruntime.NativeAcceptance{}, productruntime.ErrProtocol
	}
	operationID, err := newOperationID()
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if _, err := driver.config.MapPermission(request.PermissionMode); err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	live, err := driver.lockLive(turn.NativeSessionRef, true)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if live.permissionMode != request.PermissionMode {
		driver.mu.Unlock()
		return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedPolicy
	}
	if live.turn == nil || live.turn.id != turn.NativeTurnID {
		driver.mu.Unlock()
		return productruntime.NativeAcceptance{}, productruntime.ErrStale
	}
	accepted, err := live.client.KiloPrompt(ctx, turn.NativeSessionID, operationID, []byte(request.Prompt), "steer")
	driver.mu.Unlock()
	return accepted, err
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, turn productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	if !validNativeID(turn.NativeTurnID, "msg_") {
		return productruntime.NativeTerminal{}, productruntime.ErrProtocol
	}
	if driver.consumeInterrupted(turn) {
		return productruntime.NativeTerminal{Outcome: productruntime.TurnInterrupted, ExitLike: 130, NativeStopReason: "native-interrupt"}, nil
	}
	live, err := driver.lockLive(turn.NativeSessionRef, true)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	if live.interruptedTurnID == turn.NativeTurnID {
		live.interruptedTurnID = ""
		driver.mu.Unlock()
		return productruntime.NativeTerminal{Outcome: productruntime.TurnInterrupted, ExitLike: 130, NativeStopReason: "native-interrupt"}, nil
	}
	if live.turn == nil || live.turn.id != turn.NativeTurnID {
		driver.mu.Unlock()
		return productruntime.NativeTerminal{}, productruntime.ErrStale
	}
	if live.turn.waiting {
		driver.mu.Unlock()
		return productruntime.NativeTerminal{}, productruntime.ErrNativeRejected
	}
	operationID := live.turn.operationID
	waitCtx, cancelWait := context.WithCancel(ctx)
	live.turn.waiting = true
	live.turn.waitCancel = cancelWait
	driver.mu.Unlock()
	waitErr := live.client.WaitTurn(waitCtx, turn.NativeSessionID, turn.NativeTurnID, driver.config.DecidePermission)
	cancelWait()
	driver.mu.Lock()
	if current := driver.lanes[turn.LaneID]; current == live && current.turn != nil && current.turn.id == turn.NativeTurnID {
		current.turn.waiting = false
		current.turn.waitCancel = nil
	}
	driver.mu.Unlock()
	if waitErr != nil {
		if driver.consumeInterrupted(turn) {
			return productruntime.NativeTerminal{Outcome: productruntime.TurnInterrupted, ExitLike: 130, NativeStopReason: "native-interrupt"}, nil
		}
		if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, productruntime.ErrTimedOut) {
			return productruntime.NativeTerminal{Outcome: productruntime.TurnTimedOut, ExitLike: 124, NativeStopReason: "deadline"}, nil
		}
		if errors.Is(waitErr, context.Canceled) {
			return productruntime.NativeTerminal{}, waitErr
		}
		driver.clearTurn(turn.LaneID, operationID)
		return productruntime.NativeTerminal{Outcome: productruntime.TurnFailed, ExitLike: 1, NativeStopReason: "native-event-failure"}, nil
	}
	driver.mu.Lock()
	current := driver.lanes[turn.LaneID]
	if current == live && current.interruptedTurnID == turn.NativeTurnID {
		current.interruptedTurnID = ""
		driver.mu.Unlock()
		return productruntime.NativeTerminal{Outcome: productruntime.TurnInterrupted, ExitLike: 130, NativeStopReason: "native-interrupt"}, nil
	}
	if current != live || current.turn == nil || current.turn.id != turn.NativeTurnID {
		driver.mu.Unlock()
		return productruntime.NativeTerminal{}, productruntime.ErrStale
	}
	result, err := live.client.ResultAfter(ctx, turn.NativeSessionID, turn.NativeTurnID)
	if err != nil {
		if current.turn != nil && current.turn.operationID == operationID {
			current.turn = nil
		}
		driver.mu.Unlock()
		return productruntime.NativeTerminal{}, err
	}
	digest := sha256.Sum256([]byte(result))
	current.turn = nil
	driver.mu.Unlock()
	return productruntime.NativeTerminal{Outcome: productruntime.TurnCompleted, Result: result, ResultDigest: digest, NativeStopReason: "native-message-completed"}, nil
}

func (driver *LaneDriver) Interrupt(ctx context.Context, turn productruntime.NativeTurnRef) error {
	live, err := driver.lockLive(turn.NativeSessionRef, true)
	if err != nil {
		return err
	}
	if live.turn == nil || live.turn.id != turn.NativeTurnID {
		driver.mu.Unlock()
		return productruntime.ErrStale
	}
	if err := live.client.Interrupt(ctx, turn.NativeSessionID); err != nil {
		driver.mu.Unlock()
		return err
	}
	cancelWait := live.turn.waitCancel
	live.turn = nil
	live.interruptedTurnID = turn.NativeTurnID
	if cancelWait != nil {
		cancelWait()
	}
	driver.mu.Unlock()
	return nil
}

func (driver *LaneDriver) Archive(ctx context.Context, session productruntime.NativeSessionRef) error {
	live, err := driver.lockLive(session, false)
	if err != nil {
		return err
	}
	if live.archived {
		if live.server == nil {
			driver.mu.Unlock()
			return nil
		}
		if live.archiving {
			driver.mu.Unlock()
			return productruntime.ErrNativeRejected
		}
		live.archiving = true
		server := live.server
		driver.mu.Unlock()
		if err := server.Close(ctx); err != nil {
			driver.mu.Lock()
			live.archiving = false
			driver.mu.Unlock()
			return err
		}
		driver.mu.Lock()
		live.archiving = false
		live.client, live.server = nil, nil
		driver.mu.Unlock()
		return nil
	}
	if live.turn != nil || live.archiving {
		driver.mu.Unlock()
		return productruntime.ErrNativeRejected
	}
	live.archiving = true
	driver.mu.Unlock()
	if err := live.client.DeleteSession(ctx, session.NativeSessionID); err != nil {
		driver.mu.Lock()
		live.archiving = false
		driver.mu.Unlock()
		return err
	}
	driver.mu.Lock()
	live.archived = true
	driver.mu.Unlock()
	if err := live.server.Close(ctx); err != nil {
		driver.mu.Lock()
		live.archiving = false
		driver.mu.Unlock()
		return err
	}
	driver.mu.Lock()
	if current := driver.lanes[session.LaneID]; current == live {
		current.archiving = false
		current.client, current.server = nil, nil
	}
	driver.mu.Unlock()
	return nil
}

func (driver *LaneDriver) lockLive(ref productruntime.NativeSessionRef, requireOpen bool) (*laneSession, error) {
	driver.mu.Lock()
	if ref.Generation != driver.config.Generation || strings.TrimSpace(ref.LaneID) == "" || !validNativeID(ref.NativeSessionID, "ses_") {
		driver.mu.Unlock()
		return nil, productruntime.ErrStale
	}
	live := driver.lanes[ref.LaneID]
	if live == nil || live.provisional || live.nativeID != ref.NativeSessionID || requireOpen && (live.archived || live.archiving) || requireOpen && (live.client == nil || live.server == nil) {
		driver.mu.Unlock()
		return nil, productruntime.ErrStale
	}
	return live, nil
}

func (driver *LaneDriver) clearTurn(laneID, operationID string) {
	driver.mu.Lock()
	if live := driver.lanes[laneID]; live != nil && live.turn != nil && live.turn.operationID == operationID {
		live.turn = nil
	}
	driver.mu.Unlock()
}

func (driver *LaneDriver) consumeInterrupted(turn productruntime.NativeTurnRef) bool {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	live := driver.lanes[turn.LaneID]
	if live == nil || live.nativeID != turn.NativeSessionID || live.interruptedTurnID != turn.NativeTurnID {
		return false
	}
	live.interruptedTurnID = ""
	return true
}

func newOperationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("%w: generate native operation id", productruntime.ErrUnavailable)
	}
	return hex.EncodeToString(raw[:]), nil
}

var _ productruntime.LaneDriver = (*LaneDriver)(nil)
