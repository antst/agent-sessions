package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
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
	if product == connectorProductCodex && eventInput.Event == "SessionStart" {
		if token := strings.TrimSpace(os.Getenv(launcher.CodexPendingLaunchEnv)); token != "" {
			var values map[string]any
			if json.Unmarshal(body, &values) == nil {
				values["agent_sessions_pending_launch"] = token
				if encoded, encodeErr := json.Marshal(values); encodeErr == nil {
					body = encoded
				}
			}
		}
	}
	stateRoot := defaultStateRoot()
	endpoint, err := daemonpkg.ControlEndpoint(stateRoot)
	if err != nil {
		return nil //nolint:nilerr // An unavailable daemon must not break an ordinary vendor session.
	}
	dispatcher, err := bridge.NewHookDispatcher(bridge.HookDispatchConfig{
		Product: product, Endpoint: endpoint,
		Events:     map[string]bool{"SessionStart": true, "UserPromptSubmit": true, "Stop": true, "SessionEnd": true},
		Generation: func(context.Context) (uint64, error) { return 1, nil },
		Attest: func(_ context.Context, _ string, input json.RawMessage) (bridge.HookAttestation, error) {
			if product != "codex" {
				return bridge.HookAttestation{}, bridge.ErrConnectorInactive
			}
			threadID, identityErr := bridge.CodexHookThreadIDFromInput(input)
			if identityErr != nil {
				return bridge.HookAttestation{}, bridge.ErrConnectorInactive
			}
			var pending struct {
				Token string `json:"agent_sessions_pending_launch"`
			}
			if json.Unmarshal(input, &pending) == nil && strings.TrimSpace(pending.Token) != "" {
				caller, ancestry, ancestryErr := bridge.CaptureNativeAncestry(os.Getpid(), 24)
				if ancestryErr != nil {
					return bridge.HookAttestation{}, bridge.ErrConnectorInactive
				}
				return bridge.HookAttestation{
					AttachmentID: pending.Token, Capability: pending.Token,
					Evidence: daemonpkg.NativeEvidence{
						Process: caller, Ancestry: ancestry, ThreadID: pending.Token,
						SocketPath: filepath.Join(codexHome(), "app-server-control", "app-server-control.sock"),
					},
				}, nil
			}
			return attestHook(product, threadID)
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

func attestHook(product, threadID string) (bridge.HookAttestation, error) {
	if strings.TrimSpace(product) == "" || strings.TrimSpace(threadID) == "" {
		return bridge.HookAttestation{}, bridge.ErrConnectorInactive
	}
	return bridge.HookAttestation{
		AttachmentID: threadID,
		Evidence:     daemonpkg.NativeEvidence{ThreadID: threadID},
	}, nil
}
