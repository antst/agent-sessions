package bridge

import (
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

var (
	ErrConnectorInactive = sessiontools.ErrConnectorInactive
)

// ConnectorAttestation remains only for the product-native hook bridge. Live
// MCP calls use the session's held-open presence connection directly.
type ConnectorAttestation struct {
	AttachmentID string
	Capability   string
	Evidence     daemonpkg.NativeEvidence
}

func cloneRelayEvidence(evidence daemonpkg.NativeEvidence) daemonpkg.NativeEvidence {
	evidence.Ancestry = append([]procinfo.Identity(nil), evidence.Ancestry...)
	return evidence
}
