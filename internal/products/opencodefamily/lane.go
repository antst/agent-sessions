package opencodefamily

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	server            *LiveServer
	client            *Client
	nativeID          string
	permissionMode    permissionmode.Mode
	turn              *laneTurn
	interruptedTurnID string
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
	return productruntime.LaneCapabilitySet{DurableResume: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if request.ProductID != driver.config.ProductID || strings.TrimSpace(request.LaneID) == "" || !validDirectory(request.Cwd) ||
		!request.PermissionMode.Valid() ||
		request.ResumeNativeID == "" && strings.TrimSpace(request.Name) == "" {
		return productruntime.NativeSessionRef{}, productruntime.ErrProtocol
	}
	serverArguments := request.Arguments
	var err error
	serverArguments, _, err = splitLaneArguments(request.Arguments)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if unsafeServerArguments(serverArguments) {
		return productruntime.NativeSessionRef{}, productruntime.ErrProtocol
	}
	permissions, err := driver.config.MapPermission(request.PermissionMode)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	environment, err := productruntime.ParseNativeEnvironment(request.Environment)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	driver.mu.Lock()
	if existing := driver.lanes[request.LaneID]; existing != nil {
		if request.ResumeNativeID != "" && existing.nativeID == request.ResumeNativeID &&
			existing.permissionMode == request.PermissionMode && existing.server != nil && existing.client != nil {
			ref := productruntime.NativeSessionRef{LaneID: existing.nativeID, NativeSessionID: existing.nativeID, Generation: driver.config.Generation}
			driver.mu.Unlock()
			return ref, nil
		}
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, productruntime.ErrAmbiguousSession
	}
	live := &laneSession{nativeID: request.ResumeNativeID, permissionMode: request.PermissionMode}
	driver.lanes[request.LaneID] = live
	driver.mu.Unlock()

	server, err := driver.config.Servers.Open(ctx, ServerOpenRequest{
		Key: request.LaneID, Cwd: request.Cwd, Arguments: serverArguments, Env: environment, PermissionRules: permissions,
	})
	if server == nil {
		if err == nil {
			err = productruntime.ErrUnavailable
		}
		return productruntime.NativeSessionRef{}, driver.failOpen(context.Background(), request.LaneID, live, nil, err)
	}
	if err != nil {
		return productruntime.NativeSessionRef{}, driver.failOpen(context.Background(), request.LaneID, live, server, err)
	}
	client := server.Client()
	if client == nil {
		return productruntime.NativeSessionRef{}, driver.failOpen(context.Background(), request.LaneID, live, server, productruntime.ErrUnavailable)
	}
	var session Session
	if request.ResumeNativeID == "" {
		session, err = client.CreateSession(ctx, request.Name, permissions)
	} else {
		session, err = client.GetSession(ctx, request.ResumeNativeID)
	}
	if err != nil || request.ResumeNativeID != "" && session.ID != request.ResumeNativeID {
		if err == nil {
			err = productruntime.ErrAmbiguousSession
		}
		return productruntime.NativeSessionRef{}, driver.failOpen(context.Background(), request.LaneID, live, server, err)
	}
	driver.mu.Lock()
	if driver.lanes[request.LaneID] != live {
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, driver.failOpen(context.Background(), request.LaneID, live, server, productruntime.ErrAmbiguousSession)
	}
	if current := driver.lanes[session.ID]; current != nil && current != live {
		driver.mu.Unlock()
		return productruntime.NativeSessionRef{}, driver.failOpen(context.Background(), request.LaneID, live, server, productruntime.ErrAmbiguousSession)
	}
	live.server = server
	live.client = client
	live.nativeID = session.ID
	if request.LaneID != session.ID {
		delete(driver.lanes, request.LaneID)
		driver.lanes[session.ID] = live
	}
	driver.mu.Unlock()
	return productruntime.NativeSessionRef{LaneID: session.ID, NativeSessionID: session.ID, Generation: driver.config.Generation}, nil
}

func (driver *LaneDriver) failOpen(ctx context.Context, laneID string, live *laneSession, server *LiveServer, cause error) error {
	driver.mu.Lock()
	if driver.lanes[laneID] == live {
		delete(driver.lanes, laneID)
	}
	driver.mu.Unlock()
	if server == nil {
		return cause
	}
	return errors.Join(cause, server.Close(ctx))
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
	var model *NativeModel
	_, model, err = splitLaneArguments(request.Arguments)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	live, err := driver.lockLive(session)
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
	accepted, err := live.client.PromptAsync(ctx, session.NativeSessionID, operationID, body, false, model)
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

func splitLaneArguments(arguments []string) ([]string, *NativeModel, error) {
	serverArguments := make([]string, 0, len(arguments))
	var model *NativeModel
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		var value string
		switch {
		case argument == "--model" || argument == "-m":
			index++
			if index >= len(arguments) {
				return nil, nil, fmt.Errorf("%s requires a provider/model value", argument)
			}
			value = arguments[index]
		case strings.HasPrefix(argument, "--model="):
			value = strings.TrimPrefix(argument, "--model=")
		default:
			serverArguments = append(serverArguments, argument)
			continue
		}
		providerID, modelID, ok := strings.Cut(value, "/")
		if model != nil || !ok || !validModelPart(providerID) || !validModelPart(modelID) {
			return nil, nil, fmt.Errorf("--model requires one provider/model value")
		}
		model = &NativeModel{ProviderID: providerID, ModelID: modelID}
	}
	return serverArguments, model, nil
}

func (*LaneDriver) Steer(context.Context, productruntime.NativeTurnRef, productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedSteer
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, turn productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	if !validNativeID(turn.NativeTurnID, "msg_") {
		return productruntime.NativeTerminal{}, productruntime.ErrProtocol
	}
	if driver.consumeInterrupted(turn) {
		return productruntime.NativeTerminal{Outcome: productruntime.TurnInterrupted, ExitLike: 130, NativeStopReason: "native-interrupt"}, nil
	}
	live, err := driver.lockLive(turn.NativeSessionRef)
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
		return productruntime.NativeTerminal{Outcome: productruntime.TurnFailed, ExitLike: 1, NativeStopReason: "native-event-failure"}, waitErr
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
	live, err := driver.lockLive(turn.NativeSessionRef)
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
	live, err := driver.lockLive(session)
	if err != nil {
		return err
	}
	if live.turn != nil {
		driver.mu.Unlock()
		return productruntime.ErrNativeRejected
	}
	server := live.server
	if driver.lanes[session.LaneID] == live {
		delete(driver.lanes, session.LaneID)
	}
	driver.mu.Unlock()
	return server.Close(ctx)
}

func (driver *LaneDriver) lockLive(ref productruntime.NativeSessionRef) (*laneSession, error) {
	driver.mu.Lock()
	if ref.Generation != driver.config.Generation || strings.TrimSpace(ref.LaneID) == "" || ref.LaneID != ref.NativeSessionID || !validNativeID(ref.NativeSessionID, "ses_") {
		driver.mu.Unlock()
		return nil, productruntime.ErrStale
	}
	live := driver.lanes[ref.LaneID]
	if live == nil || live.nativeID != ref.NativeSessionID || live.client == nil || live.server == nil {
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
