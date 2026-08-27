// Package federation owns the product-neutral contracts shared by the host
// daemon and central hub. Runtime orchestration remains in federator while it
// is converged behind the unified daemon.
package federation

import (
	"fmt"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const (
	// ProtocolVersion is the exact host-to-hub compatibility version.
	ProtocolVersion = productcatalog.ProtocolVersion
	// GroupProtocolVersion is the global group-routing contract version.
	GroupProtocolVersion = 1
	// AgentFrameVersion is the product-neutral carrier body version.
	AgentFrameVersion = 1
	// MaxFrameBytes bounds one host-to-hub wire frame.
	MaxFrameBytes = 2 * 1024 * 1024
	// MaxLaneInputBytes bounds one remote lane request body.
	MaxLaneInputBytes = 1024 * 1024
	// MaxAgentFrameBytes bounds one peer-message carrier body.
	MaxAgentFrameBytes = 1024 * 1024
)

// ProtocolDescriptor returns the authoritative federation wire inventory.
func ProtocolDescriptor() productcatalog.HubProtocolDescriptor {
	return productcatalog.Catalog().HubProtocol
}

const (
	// ProtocolInventoryStart bounds the generated documentation block.
	ProtocolInventoryStart = "<!-- BEGIN: generated federation protocol inventory -->"
	// ProtocolInventoryEnd closes the generated documentation block.
	ProtocolInventoryEnd = "<!-- END: generated federation protocol inventory -->"
)

// ProtocolInventoryMarkdown renders the exact checked documentation block for
// the authoritative descriptor. Descriptive prose may surround this block,
// but protocol inventory changes must originate in productcatalog.
func ProtocolInventoryMarkdown() string {
	descriptor := ProtocolDescriptor()
	quoted := func(values []string) string {
		result := make([]string, 0, len(values))
		for _, value := range values {
			result = append(result, "`"+value+"`")
		}
		return strings.Join(result, ", ")
	}
	return fmt.Sprintf(`%s
- protocol version: %d (exact equality only; release identity is irrelevant)
- mismatch behavior: reject before registration or work acceptance
- handshake: `+"`hello` -> `hello_ok`"+`; health: `+"`probe` -> `probe_ok`"+`
- bounds: frame 2 MiB; lane input 1 MiB; AgentFrame 1 MiB
- AgentFrame version: %d
- capabilities: %s
- frame types: %s
- legacy flat `+"`deliver`"+`: rejected
%s`, ProtocolInventoryStart, descriptor.Version, descriptor.AgentFrameVersion,
		quoted(descriptor.Capabilities), quoted(descriptor.FrameTypes), ProtocolInventoryEnd)
}

// CompatibleProtocol requires exact equality. Capability advertisement is an
// operation-availability concern, not a release-version compatibility input.
func CompatibleProtocol(version int) bool { return version == ProtocolVersion }

// Host is one registered host identity and operation inventory.
type Host struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Peer is one globally addressable host-suffixed session projection.
type Peer struct {
	ID              string   `json:"id"`
	HostID          string   `json:"host_id"`
	HostName        string   `json:"host_name"`
	SessionID       string   `json:"session_id"`
	GlobalID        string   `json:"global_session_id"`
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name"`
	Status          string   `json:"status,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	Entrypoint      string   `json:"entrypoint,omitempty"`
	PermissionMode  string   `json:"permission_mode,omitempty"`
	StartedAt       int64    `json:"started_at,omitempty"`
	PeerProtocol    int      `json:"peer_protocol,omitempty"`
	InstanceID      string   `json:"instance_id,omitempty"`
	Groups          []string `json:"groups,omitempty"`
	ParentSessionID string   `json:"parent_session_id,omitempty"`
}
