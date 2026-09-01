package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

// StdioMCPThreadID extracts the Codex-owned thread identity from native MCP
// metadata. Model arguments are deliberately ignored.
func StdioMCPThreadID(params json.RawMessage) (string, error) {
	return sessiontools.StdioMCPThreadID(params)
}

// CaptureNativeAncestry returns the exact caller and bounded parent chain for
// a short-lived hook or connector process.
func CaptureNativeAncestry(startPID, limit int) (procinfo.Identity, []procinfo.Identity, error) {
	return sessiontools.CaptureNativeAncestry(startPID, limit)
}

// CodexHookThreadIDFromInput preserves the established transcript-confined
// root-thread resolution for the unified short-lived hook client.
func CodexHookThreadIDFromInput(input json.RawMessage) (string, error) {
	var decoded hookInput
	if len(input) == 0 || json.Unmarshal(input, &decoded) != nil {
		return "", ErrConnectorInactive
	}
	return codexHookThreadID(resolveNativePaths(), decoded)
}

// AttestMCPCall preserves Codex's established shared-App-Server MCP boundary:
// the relay must descend from the exact connected App Server and the exact
// thread comes from Codex-owned turn metadata, never tool arguments.
func (native *CodexNative) AttestMCPCall(startPID int, params json.RawMessage) (ConnectorAttestation, error) {
	if native == nil || native.client == nil {
		return ConnectorAttestation{}, ErrConnectorInactive
	}
	appServer := procinfo.Identity{PID: native.client.peerPID, Start: native.client.peerProcStart}
	if appServer.PID <= 1 || appServer.Start == "" || !exactProcessIdentityMatch(appServer.PID, appServer.Start) ||
		!processHasAncestor(startPID, appServer.PID) {
		return ConnectorAttestation{}, ErrConnectorInactive
	}
	threadID, err := attestStdioMCPCaller(params)
	if err != nil {
		return ConnectorAttestation{}, ErrConnectorInactive
	}
	caller, err := procinfo.CaptureIdentity(startPID)
	if err != nil {
		return ConnectorAttestation{}, ErrConnectorInactive
	}
	return ConnectorAttestation{
		AttachmentID: threadID,
		Evidence: daemonpkg.NativeEvidence{
			Process: caller, Ancestry: []procinfo.Identity{appServer}, Executable: native.config.CodexBinary,
			SocketPath: native.config.SocketPath, ThreadID: threadID,
		},
	}, nil
}

// AttestHookEvent binds one Codex hook process and transcript to the exact
// managed root thread. Bare daemon-wide hooks remain silent no-ops.
func (native *CodexNative) AttestHookEvent(
	startPID int,
	event string,
	input json.RawMessage,
) (HookAttestation, error) {
	if native == nil || native.client == nil {
		return HookAttestation{}, ErrConnectorInactive
	}
	event = strings.TrimSpace(event)
	if event != "SessionStart" && event != "UserPromptSubmit" && event != "Stop" && event != "SessionEnd" {
		return HookAttestation{}, ErrConnectorInactive
	}
	var decoded hookInput
	if len(input) == 0 || json.Unmarshal(input, &decoded) != nil {
		return HookAttestation{}, ErrConnectorInactive
	}
	if decoded.Event != "" && decoded.Event != event {
		return HookAttestation{}, ErrConnectorInactive
	}
	decoded.Event = event
	threadID, err := codexHookThreadID(native.paths, decoded)
	if err != nil {
		return HookAttestation{}, ErrConnectorInactive
	}
	caller, ancestry, err := captureNativeAncestry(startPID, 24)
	if err != nil {
		return HookAttestation{}, ErrConnectorInactive
	}
	return HookAttestation{
		AttachmentID: threadID,
		Evidence: daemonpkg.NativeEvidence{
			Process: caller, Ancestry: ancestry, Executable: native.config.CodexBinary,
			SocketPath: native.config.SocketPath, ThreadID: threadID,
		},
	}, nil
}

func captureNativeAncestry(startPID, limit int) (procinfo.Identity, []procinfo.Identity, error) {
	if startPID <= 1 || limit <= 0 {
		return procinfo.Identity{}, nil, errors.New("native process identity is unavailable")
	}
	caller, err := procinfo.CaptureIdentity(startPID)
	if err != nil {
		return procinfo.Identity{}, nil, err
	}
	ancestry := make([]procinfo.Identity, 0, limit)
	seen := map[int]bool{startPID: true}
	pid := startPID
	for len(ancestry) < limit {
		parent, ok := parentProcessID(pid)
		if !ok || parent <= 1 || seen[parent] {
			break
		}
		seen[parent] = true
		identity, captureErr := procinfo.CaptureIdentity(parent)
		if captureErr != nil {
			return procinfo.Identity{}, nil, fmt.Errorf("capture native ancestor %d: %w", parent, captureErr)
		}
		ancestry = append(ancestry, identity)
		pid = parent
	}
	return caller, ancestry, nil
}
