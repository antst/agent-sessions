//go:build darwin

package codebuddy

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func platformCommandLine(ctx context.Context, identity procinfo.Identity) ([]string, error) {
	// procinfo validates the strong start token before and after this
	// corroborating observation. ps output is never used as PID authority.
	result, err := runBoundedCommand(ctx, "/bin/ps", "-ww", "-o", "command=", "-p", strconv.Itoa(identity.PID))
	output := []byte(result.Stdout)
	if err != nil || len(output) == 0 {
		return nil, fmt.Errorf("read exact process command line: %w", err)
	}
	argv := strings.Fields(strings.TrimSpace(string(output)))
	if len(argv) == 0 {
		return nil, ErrEntrypoint
	}
	return argv, nil
}
