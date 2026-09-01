package bridge

import (
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

// Deprecated compatibility aliases. Live unified-daemon callers import
// sessiontools directly; the bridge keeps old tests and legacy entrypoints
// compiling while no new imports are permitted.
var (
	ErrConnectorInactive     = sessiontools.ErrConnectorInactive
	ErrMCPRelayFrameTooLarge = sessiontools.ErrMCPRelayFrameTooLarge
)

type ConnectorAttestation = sessiontools.ConnectorAttestation
type MCPRelayConfig = sessiontools.MCPRelayConfig
type MCPRelay = sessiontools.MCPRelay
type connectorRelayPayload = sessiontools.ConnectorRelayPayload

func NewMCPRelay(config MCPRelayConfig) (*MCPRelay, error) { return sessiontools.NewMCPRelay(config) }

func cloneRelayEvidence(evidence daemonpkg.NativeEvidence) daemonpkg.NativeEvidence {
	evidence.Ancestry = append([]procinfo.Identity(nil), evidence.Ancestry...)
	return evidence
}
