package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type SessionCandidate struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	UpdatedAt int64  `json:"updated_at"`
}

func InspectSession(ctx context.Context, executable, sessionID string, environment []string) (SessionCandidate, bool, error) {
	if ctx == nil || filepath.Base(executable) != "dsh" || strings.TrimSpace(sessionID) == "" {
		return SessionCandidate{}, false, fmt.Errorf("%w: DSH session inspection request is invalid", productruntime.ErrProtocol)
	}
	command := exec.CommandContext(ctx, executable, "--profile", ManagedProfile) //nolint:gosec // descriptor executable and exact profile.
	command.Env = envutil.Set(environment, inspectSessionEnv, sessionID)
	output, err := command.Output()
	command.Env = nil
	if err != nil {
		return SessionCandidate{}, false, fmt.Errorf("inspect DSH session %q: %w", sessionID, err)
	}
	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	if len(lines) != 1 {
		return SessionCandidate{}, false, fmt.Errorf("%w: DSH session inspection returned %d frames", productruntime.ErrProtocol, len(lines))
	}
	var result struct {
		Found *bool `json:"found"`
		SessionCandidate
	}
	decoder := json.NewDecoder(bytes.NewReader(lines[0]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.Found == nil {
		return SessionCandidate{}, false, fmt.Errorf("%w: DSH session inspection result is invalid", productruntime.ErrProtocol)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return SessionCandidate{}, false, fmt.Errorf("%w: DSH session inspection result has trailing data", productruntime.ErrProtocol)
	}
	if !*result.Found {
		return SessionCandidate{}, false, nil
	}
	if result.SessionID != sessionID {
		return SessionCandidate{}, false, fmt.Errorf("%w: DSH session inspection changed identity", productruntime.ErrProtocol)
	}
	return result.SessionCandidate, true, nil
}
