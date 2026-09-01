//go:build linux

package codebuddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func platformCommandLine(ctx context.Context, identity procinfo.Identity) ([]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	path := "/proc/" + strconv.Itoa(identity.PID) + "/cmdline"
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read exact process command line: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	err = errors.Join(readErr, closeErr)
	if err != nil || len(body) == 0 || len(body) > 1<<20 {
		return nil, fmt.Errorf("read exact process command line: %w", err)
	}
	parts := bytes.Split(bytes.TrimSuffix(body, []byte{0}), []byte{0})
	argv := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		argv = append(argv, string(part))
	}
	if len(argv) == 0 {
		return nil, ErrEntrypoint
	}
	return argv, nil
}
