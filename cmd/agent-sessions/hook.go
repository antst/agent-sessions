package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

const maxHookInputBytes = 2 << 20

func runHook(ctx context.Context, product string, output io.Writer) error {
	return runHookInput(ctx, product, os.Stdin, output)
}

func runHookInput(ctx context.Context, product string, input io.Reader, output io.Writer) error {
	body, err := io.ReadAll(io.LimitReader(input, maxHookInputBytes+1))
	if err != nil || len(body) > maxHookInputBytes || !json.Valid(body) {
		// Vendor-wide hooks must never damage an ordinary session. Invalid or
		// unattested input is therefore the same silent no-op as a bare session.
		return nil //nolint:nilerr // Invalid vendor hook input is deliberately a silent no-op.
	}
	var eventInput struct {
		Event string `json:"hook_event_name"`
	}
	if json.Unmarshal(body, &eventInput) != nil || strings.TrimSpace(eventInput.Event) == "" {
		return nil //nolint:nilerr // Unrecognized vendor hook input is deliberately a silent no-op.
	}
	stateRoot := defaultStateRoot()
	endpoint, err := daemonpkg.ControlEndpoint(stateRoot)
	if err != nil {
		return nil //nolint:nilerr // An unavailable daemon must not break an ordinary vendor session.
	}
	dispatcher, err := bridge.NewHookDispatcher(bridge.HookDispatchConfig{
		Product: product, Endpoint: endpoint,
		Events: map[string]bool{"SessionStart": true, "UserPromptSubmit": true, "Stop": true, "SessionEnd": true},
		Generation: func(context.Context) (uint64, error) {
			state, openErr := daemonpkg.OpenState(stateRoot, 16<<20)
			if openErr != nil {
				return 0, openErr
			}
			snapshot, readErr := state.Read()
			return snapshot.Catalog.Host.Generation, readErr
		},
		Attest: func(_ context.Context, _ string, input json.RawMessage) (bridge.HookAttestation, error) {
			if product != "codex" {
				return bridge.HookAttestation{}, bridge.ErrConnectorInactive
			}
			threadID, identityErr := bridge.CodexHookThreadIDFromInput(input)
			if identityErr != nil {
				return bridge.HookAttestation{}, bridge.ErrConnectorInactive
			}
			return attestHookFromState(stateRoot, product, threadID, os.Getpid())
		},
	})
	if err != nil {
		return nil //nolint:nilerr // An invalid inactive dispatcher must not break an ordinary vendor session.
	}
	result, err := dispatcher.Dispatch(ctx, eventInput.Event, json.RawMessage(body))
	if err != nil {
		// A daemon restart or stale non-peer snapshot must not break Codex's
		// native session. The hook has no lifecycle authority of its own.
		return nil //nolint:nilerr // Restart and non-peer hook snapshots are deliberately silent no-ops.
	}
	if len(result) != 0 {
		_, _ = output.Write(append(result, '\n'))
	}
	return nil
}

func attestHookFromState(stateRoot, product, threadID string, pid int) (bridge.HookAttestation, error) {
	caller, ancestry, err := bridge.CaptureNativeAncestry(pid, 32)
	if err != nil {
		return bridge.HookAttestation{}, bridge.ErrConnectorInactive
	}
	state, err := daemonpkg.OpenState(stateRoot, 16<<20)
	if err != nil {
		return bridge.HookAttestation{}, bridge.ErrConnectorInactive
	}
	snapshot, err := state.Read()
	if err != nil {
		return bridge.HookAttestation{}, bridge.ErrConnectorInactive
	}
	attachment, ok := snapshot.Catalog.Attachments[threadID]
	if !ok || attachment.Product != product || attachment.NativeSessionID != threadID || attachment.State != "attached" ||
		attachment.DaemonGeneration != snapshot.Catalog.Host.Generation {
		return bridge.HookAttestation{}, bridge.ErrConnectorInactive
	}
	evidence := daemonpkg.NativeEvidence{
		Process: caller, Ancestry: ancestry, ThreadID: threadID,
		SocketPath: attachment.Evidence.SocketPath, Executable: attachment.Evidence.Executable,
	}
	return bridge.HookAttestation{AttachmentID: attachment.ID, Evidence: evidence}, nil
}
