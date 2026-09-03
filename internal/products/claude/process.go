package claude

import (
	"context"
	"errors"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/structuredprocess"
)

type streamProcess interface {
	ReadFrame(context.Context) ([]byte, error)
	WriteFrame(context.Context, []byte) error
	Cleanup(context.Context) error
}

type ProcessFactory interface {
	StartStream(context.Context, productruntime.NativeCommand) (streamProcess, error)
}

type structuredFactory struct{ supervisor *structuredprocess.Supervisor }

func NewStructuredProcessFactory(supervisor *structuredprocess.Supervisor) (ProcessFactory, error) {
	if supervisor == nil {
		return nil, errors.New("Claude structured process supervisor is nil")
	}
	return structuredFactory{supervisor: supervisor}, nil
}

func (factory structuredFactory) StartStream(ctx context.Context, command productruntime.NativeCommand) (streamProcess, error) {
	return factory.supervisor.StartProcess(ctx, command)
}
