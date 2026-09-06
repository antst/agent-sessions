package grok

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antst/sessionbus/internal/permissionmode"
	"github.com/antst/sessionbus/internal/productcatalog"
	"github.com/antst/sessionbus/internal/productruntime"
	"github.com/antst/sessionbus/internal/sessiontools"
)

const ProductID = "grok"

type NativePrompt interface {
	Wait(context.Context) (NativePromptResult, error)
}

type NativePromptResult struct {
	Output     string
	StopReason string
}

type NativeSession interface {
	NativeID() string
	SetModel(context.Context, string) error
	SetMode(context.Context, string) error
	StartPrompt(context.Context, string) (NativePrompt, error)
	Interject(context.Context, string, string) error
	Cancel() error
	Close()
}

type NativeOpenRequest struct {
	LaneID, Name, ResumeNativeID, Cwd, Capability string
	PermissionMode                                string
	Groups                                        []string
	Arguments                                     []string
	Environment                                   []string
}

type NativeFactory interface {
	Open(context.Context, NativeOpenRequest) (NativeSession, error)
}

type LaneConfig struct {
	Descriptor productcatalog.Descriptor
	Generation uint64
	Native     NativeFactory
	Now        func() time.Time
}

type LaneDriver struct {
	config LaneConfig
	mu     sync.Mutex
	lanes  map[string]*laneSession
}

type laneSession struct {
	mu         sync.Mutex
	ref        productruntime.NativeSessionRef
	permission permissionmode.Mode
	native     NativeSession
	active     *laneTurn
	nextID     uint64
	closed     bool
}

type laneTurn struct {
	ref    productruntime.NativeTurnRef
	prompt NativePrompt
}

func NewLaneDriver(config LaneConfig) (*LaneDriver, error) {
	if config.Descriptor.ID != ProductID || strings.TrimSpace(config.Descriptor.NativeExecutable) == "" ||
		config.Generation == 0 || config.Native == nil {
		return nil, errors.New("Grok lane requires its descriptor, generation, and native factory")
	}
	if len(config.Descriptor.NativeYoloArgs) != 1 || config.Descriptor.NativeYoloArgs[0] != "--yolo" {
		return nil, errors.New("Grok lane descriptor is missing literal native --yolo")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &LaneDriver{config: config, lanes: make(map[string]*laneSession)}, nil
}

func (*LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{Steer: true, DurableResume: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if ctx == nil {
		return productruntime.NativeSessionRef{}, errors.New("Grok lane open requires context")
	}
	if err := ctx.Err(); err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if request.ProductID != ProductID || strings.TrimSpace(request.LaneID) == "" || strings.TrimSpace(request.Cwd) == "" ||
		strings.TrimSpace(request.Capability) == "" || request.PermissionMode != permissionmode.BypassPermissions {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: Grok lanes require explicit --yolo", productruntime.ErrUnsupportedPolicy)
	}
	if request.ResumeNativeID == "" && strings.TrimSpace(request.Name) == "" {
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: fresh Grok lane requires a product-native name", productruntime.ErrNativeRejected)
	}
	model, arguments, err := extractModel(request.Arguments)
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	for _, argument := range arguments {
		if reservedArgument(argument) {
			return productruntime.NativeSessionRef{}, fmt.Errorf("%w: native argument %q is owned by the Grok lane lifecycle", productruntime.ErrUnsupportedPolicy, argument)
		}
	}

	driver.mu.Lock()
	if existing := driver.lanes[request.LaneID]; existing != nil {
		existing.mu.Lock()
		live := !existing.closed && request.ResumeNativeID != "" && existing.ref.NativeSessionID == request.ResumeNativeID &&
			existing.permission == request.PermissionMode
		ref, native := existing.ref, existing.native
		existing.mu.Unlock()
		driver.mu.Unlock()
		if !live {
			return productruntime.NativeSessionRef{}, fmt.Errorf("%w: lane %q already has an ephemeral Grok client", productruntime.ErrAmbiguousSession, request.LaneID)
		}
		if model != "" {
			if err := native.SetModel(ctx, model); err != nil {
				return productruntime.NativeSessionRef{}, err
			}
		}
		return ref, nil
	}
	driver.mu.Unlock()

	native, err := driver.config.Native.Open(ctx, NativeOpenRequest{
		LaneID: request.LaneID, Name: request.Name, ResumeNativeID: request.ResumeNativeID,
		Cwd: request.Cwd, Capability: request.Capability, Groups: append([]string(nil), request.Groups...),
		Arguments: arguments, Environment: append([]string(nil), request.Environment...), PermissionMode: string(request.PermissionMode),
	})
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	nativeID := native.NativeID()
	if strings.TrimSpace(nativeID) == "" || request.ResumeNativeID != "" && nativeID != request.ResumeNativeID {
		native.Close()
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: Grok selected a different native session", productruntime.ErrAmbiguousSession)
	}
	if model != "" {
		if err := native.SetModel(ctx, model); err != nil {
			native.Close()
			return productruntime.NativeSessionRef{}, err
		}
	}
	ref := productruntime.NativeSessionRef{LaneID: nativeID, NativeSessionID: nativeID, Generation: driver.config.Generation}
	session := &laneSession{ref: ref, permission: request.PermissionMode, native: native}
	driver.mu.Lock()
	if _, exists := driver.lanes[nativeID]; exists {
		driver.mu.Unlock()
		native.Close()
		return productruntime.NativeSessionRef{}, fmt.Errorf("%w: Grok native session %q already has a lane owner", productruntime.ErrAmbiguousSession, nativeID)
	}
	driver.lanes[nativeID] = session
	driver.mu.Unlock()
	return ref, nil
}

func (driver *LaneDriver) StartTurn(ctx context.Context, ref productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	session, err := driver.session(ref)
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if request.PermissionMode != session.permission {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: per-turn policy differs from the Grok launch policy", productruntime.ErrUnsupportedPolicy)
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: Grok lane prompt is empty", productruntime.ErrNativeRejected)
	}
	if request.Effort != "" {
		if err := session.native.SetMode(ctx, request.Effort); err != nil {
			return productruntime.NativeTurnRef{}, err
		}
	}
	session.mu.Lock()
	if session.closed || session.active != nil {
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, fmt.Errorf("%w: Grok lane is not idle", productruntime.ErrAmbiguousSession)
	}
	session.nextID++
	turnRef := productruntime.NativeTurnRef{NativeSessionRef: ref, NativeTurnID: strconv.FormatUint(session.nextID, 10)}
	prompt, err := session.native.StartPrompt(ctx, request.Prompt)
	if err != nil {
		session.mu.Unlock()
		return productruntime.NativeTurnRef{}, err
	}
	session.active = &laneTurn{ref: turnRef, prompt: prompt}
	session.mu.Unlock()
	return turnRef, nil
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, ref productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	session, turn, err := driver.turn(ref)
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	result, waitErr := turn.prompt.Wait(ctx)
	if waitErr != nil {
		if ctx != nil && ctx.Err() != nil {
			return productruntime.NativeTerminal{}, ctx.Err()
		}
		driver.clearTurn(session, turn)
		return productruntime.NativeTerminal{Outcome: productruntime.TurnFailed, ExitLike: 1, NativeStopReason: "native-error"}, waitErr
	}
	driver.clearTurn(session, turn)
	terminal := productruntime.NativeTerminal{
		Outcome: productruntime.TurnCompleted, Result: result.Output,
		ResultDigest: sha256.Sum256([]byte(result.Output)), NativeStopReason: result.StopReason,
	}
	if result.StopReason == "cancelled" {
		terminal.Outcome, terminal.ExitLike = productruntime.TurnInterrupted, 130
	}
	return terminal, nil
}

func (driver *LaneDriver) Steer(ctx context.Context, ref productruntime.NativeTurnRef, request productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	session, _, err := driver.turn(ref)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if request.PermissionMode != session.permission || strings.TrimSpace(request.Prompt) == "" {
		return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedPolicy
	}
	messageID := session.nextMessageID("steer")
	if err := session.native.Interject(ctx, messageID, request.Prompt); err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	return productruntime.NativeAcceptance{
		NativeSessionID: ref.NativeSessionID, NativeMessageID: messageID, AcceptedAt: driver.config.Now().UTC(),
	}, nil
}

func (driver *LaneDriver) SendMessage(ctx context.Context, ref productruntime.NativeSessionRef, message productruntime.NativeMessage) error {
	session, err := driver.session(ref)
	if err != nil {
		return err
	}
	rendered, err := sessiontools.RenderNativeMessage(message)
	if err != nil {
		return productruntime.ErrProtocol
	}
	return session.native.Interject(ctx, session.nextMessageID("message"), rendered)
}

func (driver *LaneDriver) Interrupt(_ context.Context, ref productruntime.NativeTurnRef) error {
	session, _, err := driver.turn(ref)
	if err != nil {
		return err
	}
	return session.native.Cancel()
}

func (driver *LaneDriver) Archive(_ context.Context, ref productruntime.NativeSessionRef) error {
	session, err := driver.lookupSession(ref)
	if err != nil {
		return err
	}
	session.mu.Lock()
	if session.active != nil {
		session.mu.Unlock()
		return fmt.Errorf("%w: refuse to archive a running Grok turn", productruntime.ErrNativeRejected)
	}
	session.closed = true
	session.mu.Unlock()
	session.native.Close()
	driver.mu.Lock()
	if driver.lanes[ref.LaneID] == session {
		delete(driver.lanes, ref.LaneID)
	}
	driver.mu.Unlock()
	return nil
}

func (driver *LaneDriver) session(ref productruntime.NativeSessionRef) (*laneSession, error) {
	session, err := driver.lookupSession(ref)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	if closed {
		return nil, productruntime.ErrStale
	}
	return session, nil
}

func (driver *LaneDriver) lookupSession(ref productruntime.NativeSessionRef) (*laneSession, error) {
	if ref.Generation != driver.config.Generation || strings.TrimSpace(ref.LaneID) == "" || ref.LaneID != ref.NativeSessionID {
		return nil, productruntime.ErrStale
	}
	driver.mu.Lock()
	session := driver.lanes[ref.LaneID]
	driver.mu.Unlock()
	if session == nil || session.ref != ref {
		return nil, productruntime.ErrStale
	}
	return session, nil
}

func (driver *LaneDriver) turn(ref productruntime.NativeTurnRef) (*laneSession, *laneTurn, error) {
	session, err := driver.session(ref.NativeSessionRef)
	if err != nil {
		return nil, nil, err
	}
	session.mu.Lock()
	turn := session.active
	session.mu.Unlock()
	if turn == nil || turn.ref != ref {
		return nil, nil, productruntime.ErrStale
	}
	return session, turn, nil
}

func (*LaneDriver) clearTurn(session *laneSession, turn *laneTurn) {
	session.mu.Lock()
	if session.active == turn {
		session.active = nil
	}
	session.mu.Unlock()
}

func (session *laneSession) nextMessageID(kind string) string {
	session.mu.Lock()
	session.nextID++
	id := session.nextID
	session.mu.Unlock()
	return kind + "-" + strconv.FormatUint(id, 10)
}

func extractModel(arguments []string) (string, []string, error) {
	model := ""
	result := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--model" || argument == "-m":
			if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
				return "", nil, errors.New("--model requires a non-empty value")
			}
			model = arguments[index+1]
			index++
		case strings.HasPrefix(argument, "--model=") || strings.HasPrefix(argument, "-m="):
			model = strings.SplitN(argument, "=", 2)[1]
			if strings.TrimSpace(model) == "" {
				return "", nil, errors.New("--model requires a non-empty value")
			}
		default:
			result = append(result, argument)
		}
	}
	return model, result, nil
}

func reservedArgument(argument string) bool {
	argument = strings.SplitN(argument, "=", 2)[0]
	switch argument {
	case "--leader-socket", "--leader", "--no-leader", "--session-id", "--resume", "--cwd",
		"--permission-mode", "--always-approve", "--yolo", "agent", "leader", "stdio":
		return true
	default:
		return false
	}
}
