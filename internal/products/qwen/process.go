package qwen

import (
	"context"
	"errors"

	"github.com/antst/sessionbus/internal/productruntime"
	"github.com/antst/sessionbus/internal/structuredprocess"
)

type structuredFactory struct{ supervisor *structuredprocess.Supervisor }

func NewStructuredProcessFactory(supervisor *structuredprocess.Supervisor) (ProcessFactory, error) {
	if supervisor == nil {
		return nil, errors.New("Qwen structured process supervisor is nil")
	}
	return structuredFactory{supervisor: supervisor}, nil
}

func (factory structuredFactory) StartRPC(ctx context.Context, command productruntime.NativeCommand) (rpcProcess, error) {
	return factory.supervisor.StartProcess(ctx, command)
}
