//go:build !linux && !darwin

package codebuddy

import (
	"context"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func platformCommandLine(context.Context, procinfo.Identity) ([]string, error) {
	return nil, ErrEntrypoint
}
