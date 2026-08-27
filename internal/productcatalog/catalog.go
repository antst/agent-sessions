// Package productcatalog owns the closed inventory shared by the host daemon,
// hub, launchers, adapters, release tooling, and protocol documentation.
package productcatalog

import "sort"

const (
	// ProtocolVersion is the sole software-version interoperability input for
	// host-to-hub federation. Releases with this exact value interoperate.
	ProtocolVersion = 3

	// CapabilityCodexLane identifies Codex lane operation availability.
	CapabilityCodexLane = "codex-lane"
	// CapabilityClaudeLane identifies Claude lane operation availability.
	CapabilityClaudeLane = "claude-lane"
	// CapabilityGrokLane identifies Grok lane operation availability.
	CapabilityGrokLane = "grok-lane"
	// CapabilityQwenLane identifies Qwen lane operation availability.
	CapabilityQwenLane = "qwen-lane"
)

// BinaryRole separates the host authority image from the central hub image.
type BinaryRole string

const (
	// BinaryRoleHost identifies the one per-user host daemon image.
	BinaryRoleHost BinaryRole = "host"
	// BinaryRoleHub identifies the one central network hub image.
	BinaryRoleHub BinaryRole = "hub"
)

// BinaryDescriptor is one independently deployed executable role.
type BinaryDescriptor struct {
	Name string
	Role BinaryRole
}

// ConnectorDescriptor defines one vendor's stateless relay installation.
type ConnectorDescriptor struct {
	Type         string
	ManifestPath string
	EntryPoint   string
	Mode         string
}

// ProductDescriptor is the authoritative product and connector projection.
// Existing launch and adapter code derives its local views from these canonical
// fields instead of maintaining parallel descriptor tables.
type ProductDescriptor struct {
	ID                    string
	Label                 string
	PeerAlias             string
	LaneAlias             string
	LaneCapability        string
	Connector             ConnectorDescriptor
	LaneRuntimeRole       string
	LaneManagerRole       string
	DynamicPermission     bool
	TranscriptNameIndex   bool
	InteractiveResumeFlag bool
}

// SupportsResume reports whether the product supports the requested actor kind.
func (p ProductDescriptor) SupportsResume(kind string) bool {
	return kind == "interactive" || kind == "lane"
}

// ResumeArguments returns the product-native selector for an exact session.
func (p ProductDescriptor) ResumeArguments(kind, sessionID string) ([]string, bool) {
	if !p.SupportsResume(kind) || sessionID == "" {
		return nil, false
	}
	if kind == "lane" || !p.InteractiveResumeFlag {
		return []string{"resume", sessionID}, true
	}
	return []string{"--resume", sessionID}, true
}

// HubProtocolDescriptor is the exact host-to-hub wire inventory.
type HubProtocolDescriptor struct {
	Version                  int
	MaxFrameBytes            int
	MaxLaneInputBytes        int
	MaxAgentFrameBytes       int
	Capabilities             []string
	FrameTypes               []string
	AgentFrameVersion        int
	RejectLegacyFlatDelivery bool
	ReleaseCoupled           bool
}

// CatalogDescriptor is the closed product, binary, alias, and protocol catalog.
type CatalogDescriptor struct {
	Products           []ProductDescriptor
	Binaries           []BinaryDescriptor
	ReleaseExecutables []string
	HostAliases        []string
	HubProtocol        HubProtocolDescriptor
}

var products = [...]ProductDescriptor{
	{
		ID: "codex", Label: "Codex", PeerAlias: "codex-peer", LaneAlias: "codex-peer-lane",
		LaneCapability:  CapabilityCodexLane,
		Connector:       ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: ".mcp.json", EntryPoint: "scripts/native-entry", Mode: "mcp"},
		LaneRuntimeRole: "lane",
	},
	{
		ID: "claude", Label: "Claude Code", PeerAlias: "claude-peer", LaneAlias: "claude-peer-lane",
		LaneCapability:  CapabilityClaudeLane,
		Connector:       ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: "claude/.mcp.json", EntryPoint: "agent-sessions", Mode: "mcp"},
		LaneRuntimeRole: "claude-lane", LaneManagerRole: "claude-lane-manager",
		TranscriptNameIndex: true, InteractiveResumeFlag: true,
	},
	{
		ID: "grok", Label: "Grok", PeerAlias: "grok-peer", LaneAlias: "grok-peer-lane",
		LaneCapability:  CapabilityGrokLane,
		Connector:       ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: "grok/.mcp.json", EntryPoint: "grok/scripts/native-entry", Mode: "mcp"},
		LaneRuntimeRole: "grok-lane", LaneManagerRole: "grok-lane-manager",
		DynamicPermission: true, InteractiveResumeFlag: true,
	},
	{
		ID: "qwen", Label: "Qwen Code", PeerAlias: "qwen-peer", LaneAlias: "qwen-peer-lane",
		LaneCapability:  CapabilityQwenLane,
		Connector:       ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: "qwen/mcp.json", EntryPoint: "qwen/scripts/native-entry", Mode: "mcp"},
		LaneRuntimeRole: "qwen-lane", LaneManagerRole: "qwen-lane-manager",
		DynamicPermission: true, InteractiveResumeFlag: true,
	},
}

var protocol = HubProtocolDescriptor{
	Version: ProtocolVersion, MaxFrameBytes: 2 * 1024 * 1024,
	MaxLaneInputBytes: 1024 * 1024, MaxAgentFrameBytes: 1024 * 1024,
	FrameTypes: []string{
		"hello", "hello_ok", "probe", "probe_ok", "snapshot", "roster",
		"group_deliver", "terminal_notice_deliver", "delivery_ack", "delivery_error",
		"lane_exec", "lane_cancel", "lane_stdout", "lane_stderr", "lane_exit", "lane_error",
		"ping", "pong",
	},
	AgentFrameVersion: 1, RejectLegacyFlatDelivery: true,
}

// Catalog returns an independent authoritative inventory projection.
func Catalog() CatalogDescriptor {
	productCopy := append([]ProductDescriptor(nil), products[:]...)
	binaries := []BinaryDescriptor{{Name: "agent-sessions", Role: BinaryRoleHost}, {Name: "agent-sessions-hub", Role: BinaryRoleHub}}
	releaseExecutables := make([]string, 0, len(binaries))
	for _, binary := range binaries {
		releaseExecutables = append(releaseExecutables, binary.Name)
	}
	hostAliases := make([]string, 1, 1+2*len(productCopy))
	hostAliases[0] = "peer"
	capabilities := make([]string, 0, len(productCopy))
	for _, product := range productCopy {
		hostAliases = append(hostAliases, product.PeerAlias)
		capabilities = append(capabilities, product.LaneCapability)
	}
	for _, product := range productCopy {
		hostAliases = append(hostAliases, product.LaneAlias)
	}
	sort.Strings(capabilities)
	protocolCopy := protocol
	protocolCopy.Capabilities = capabilities
	protocolCopy.FrameTypes = append([]string(nil), protocol.FrameTypes...)
	return CatalogDescriptor{
		Products:           productCopy,
		Binaries:           binaries,
		ReleaseExecutables: releaseExecutables,
		HostAliases:        hostAliases,
		HubProtocol:        protocolCopy,
	}
}

// ProductDescriptors returns an independent copy of every supported product.
func ProductDescriptors() []ProductDescriptor {
	return append([]ProductDescriptor(nil), products[:]...)
}

// ProductByID resolves one canonical product identifier.
func ProductByID(id string) (ProductDescriptor, bool) {
	for _, product := range products {
		if product.ID == id {
			return product, true
		}
	}
	return ProductDescriptor{}, false
}

// ProductByCapability resolves one lane operation capability.
func ProductByCapability(capability string) (ProductDescriptor, bool) {
	for _, product := range products {
		if product.LaneCapability == capability {
			return product, true
		}
	}
	return ProductDescriptor{}, false
}

// ResolveExecutable maps a shipped binary or host alias to its owning image.
func (c CatalogDescriptor) ResolveExecutable(name string) (string, bool) {
	for _, binary := range c.Binaries {
		if name == binary.Name {
			return binary.Name, true
		}
	}
	for _, alias := range c.HostAliases {
		if alias == name {
			return "agent-sessions", true
		}
	}
	return "", false
}
