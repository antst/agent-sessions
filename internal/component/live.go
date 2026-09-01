package component

import (
	"context"

	"github.com/antst/agent-sessions/internal/procinfo"
)

// ComponentSocketName is the conventional live component socket name.
const ComponentSocketName = "component.sock"

// BindingView identifies one process-local component connection. The view is
// deliberately ephemeral; disconnecting the socket removes it.
type BindingView struct {
	BindingID       string
	AttachmentID    string
	ProductID       string
	ProcessIdentity procinfo.Identity
	Generation      uint64
}

// Handler receives one decoded frame from a live component connection.
type Handler interface {
	HandleComponentFrame(context.Context, BindingView, Frame) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, BindingView, Frame) error

func (f HandlerFunc) HandleComponentFrame(ctx context.Context, binding BindingView, frame Frame) error {
	return f(ctx, binding, frame)
}

// RenameFailureCategory is the component-local result of a live rename call.
type RenameFailureCategory string

const (
	RenameUnsupported    RenameFailureCategory = "unsupported"
	RenameUnavailable    RenameFailureCategory = "unavailable"
	RenameTimedOut       RenameFailureCategory = "timed-out"
	RenameNativeRejected RenameFailureCategory = "native-rejected"
	RenameProtocol       RenameFailureCategory = "protocol"
)

// RenameError carries a stable component-local category and bounded detail.
type RenameError struct {
	Category RenameFailureCategory
	Detail   string
}

func (e *RenameError) Error() string {
	if e.Detail == "" {
		return string(e.Category)
	}
	return string(e.Category) + ": " + e.Detail
}

var _ error = (*RenameError)(nil)
