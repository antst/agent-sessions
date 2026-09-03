package codex

import (
	"context"
	"errors"
	"strings"

	"github.com/antst/agent-sessions/internal/bridge"
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const ProductID = "codex"

// LaneNative is the exact Codex App Server surface used by one lane driver.
type LaneNative interface {
	StartThread(context.Context, bridge.CodexStartRequest) (bridge.CodexNativeThread, error)
	PrepareLaneThread(context.Context, string, string, string, string) (bridge.CodexNativeThread, error)
	StartLaneTurn(context.Context, bridge.CodexLaneTurnRequest) (string, error)
	WaitLaneTurn(context.Context, string, string) (bridge.CodexLaneTurnResult, error)
	InterruptLaneTurn(context.Context, string, string) error
	SendMessage(context.Context, string, string) (string, error)
	ArchiveThread(context.Context, string) error
}

type NativeProvider func() (LaneNative, error)

// LaneDriver translates the product-neutral lane contract to Codex App Server.
// It stores no sessions or turns; the product owns both.
type LaneDriver struct {
	native NativeProvider
}

func NewLaneDriver(native NativeProvider) (*LaneDriver, error) {
	if native == nil {
		return nil, errors.New("codex lane native provider is unavailable")
	}
	return &LaneDriver{native: native}, nil
}

func (*LaneDriver) Capabilities() productruntime.LaneCapabilitySet {
	return productruntime.LaneCapabilitySet{DurableResume: true}
}

func (driver *LaneDriver) Open(ctx context.Context, request productruntime.LaneOpenRequest) (productruntime.NativeSessionRef, error) {
	if request.ProductID != ProductID || strings.TrimSpace(request.LaneID) == "" || strings.TrimSpace(request.Cwd) == "" || !request.PermissionMode.Valid() {
		return productruntime.NativeSessionRef{}, productruntime.ErrProtocol
	}
	approval, sandbox := codexPolicy(request.PermissionMode, request.ApprovalPolicy, request.Sandbox)
	native, err := driver.native()
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	var thread bridge.CodexNativeThread
	if request.ResumeNativeID == "" {
		if strings.TrimSpace(request.Name) == "" {
			return productruntime.NativeSessionRef{}, productruntime.ErrProtocol
		}
		thread, err = native.StartThread(ctx, bridge.CodexStartRequest{
			Cwd: request.Cwd, Name: request.Name, NameSource: "lane",
			ApprovalPolicy: approval, Sandbox: sandbox,
		})
	} else {
		thread, err = native.PrepareLaneThread(ctx, request.ResumeNativeID, request.Cwd, approval, sandbox)
	}
	if err != nil {
		return productruntime.NativeSessionRef{}, err
	}
	if strings.TrimSpace(thread.ID) == "" || request.ResumeNativeID != "" && thread.ID != request.ResumeNativeID {
		return productruntime.NativeSessionRef{}, errors.New("codex App Server selected a different thread")
	}
	return productruntime.NativeSessionRef{LaneID: request.LaneID, NativeSessionID: thread.ID, Generation: 1}, nil
}

func (driver *LaneDriver) StartTurn(ctx context.Context, session productruntime.NativeSessionRef, request productruntime.TurnStartRequest) (productruntime.NativeTurnRef, error) {
	if session.LaneID == "" || session.NativeSessionID == "" || session.Generation != 1 || strings.TrimSpace(request.Prompt) == "" || !request.PermissionMode.Valid() {
		return productruntime.NativeTurnRef{}, productruntime.ErrProtocol
	}
	approval, sandbox := codexPolicy(request.PermissionMode, request.ApprovalPolicy, request.Sandbox)
	native, err := driver.native()
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	turnID, err := native.StartLaneTurn(ctx, bridge.CodexLaneTurnRequest{
		ThreadID: session.NativeSessionID, Prompt: request.Prompt, Effort: request.Effort,
		ApprovalPolicy: approval, Sandbox: sandbox, SchemaPath: request.SchemaPath,
		Arguments: append([]string(nil), request.Arguments...),
	})
	if err != nil {
		return productruntime.NativeTurnRef{}, err
	}
	if strings.TrimSpace(turnID) == "" {
		return productruntime.NativeTurnRef{}, errors.New("codex App Server returned no turn identity")
	}
	return productruntime.NativeTurnRef{NativeSessionRef: session, NativeTurnID: turnID}, nil
}

func (driver *LaneDriver) WaitTurn(ctx context.Context, turn productruntime.NativeTurnRef) (productruntime.NativeTerminal, error) {
	if turn.Generation != 1 || turn.NativeSessionID == "" || turn.NativeTurnID == "" {
		return productruntime.NativeTerminal{}, productruntime.ErrProtocol
	}
	native, err := driver.native()
	if err != nil {
		return productruntime.NativeTerminal{}, err
	}
	result, err := native.WaitLaneTurn(ctx, turn.NativeSessionID, turn.NativeTurnID)
	if err == nil && (result.ThreadID != "" && result.ThreadID != turn.NativeSessionID || result.TurnID != "" && result.TurnID != turn.NativeTurnID) {
		return productruntime.NativeTerminal{}, errors.New("codex terminal changed native identity")
	}
	return productruntime.NativeTerminal{Outcome: productruntime.TurnOutcome(result.Outcome), Result: result.Result}, err
}

func (*LaneDriver) Steer(context.Context, productruntime.NativeTurnRef, productruntime.TurnStartRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrUnsupportedSteer
}

func (driver *LaneDriver) Interrupt(ctx context.Context, turn productruntime.NativeTurnRef) error {
	if turn.Generation != 1 || turn.NativeSessionID == "" || turn.NativeTurnID == "" {
		return productruntime.ErrProtocol
	}
	native, err := driver.native()
	if err != nil {
		return err
	}
	return native.InterruptLaneTurn(ctx, turn.NativeSessionID, turn.NativeTurnID)
}

func (driver *LaneDriver) SendMessage(ctx context.Context, session productruntime.NativeSessionRef, message string) error {
	if session.Generation != 1 || session.NativeSessionID == "" || strings.TrimSpace(message) == "" {
		return productruntime.ErrProtocol
	}
	native, err := driver.native()
	if err != nil {
		return err
	}
	_, err = native.SendMessage(ctx, session.NativeSessionID, message)
	return err
}

func (driver *LaneDriver) Archive(ctx context.Context, session productruntime.NativeSessionRef) error {
	if session.Generation != 1 || session.NativeSessionID == "" {
		return productruntime.ErrProtocol
	}
	native, err := driver.native()
	if err != nil {
		return err
	}
	return native.ArchiveThread(ctx, session.NativeSessionID)
}

func codexPolicy(mode permissionmode.Mode, approval, sandbox string) (string, string) {
	if mode == permissionmode.BypassPermissions {
		return "never", "danger-full-access"
	}
	return approval, sandbox
}

var _ productruntime.LaneDriver = (*LaneDriver)(nil)
var _ productruntime.LaneMessageDriver = (*LaneDriver)(nil)
