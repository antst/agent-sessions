// Package productcatalog defines the closed inventory of native products
// supported by Agent Sessions. Product-specific behavior belongs in adapters;
// shared help, packaging, launcher, lane, and federation surfaces consume this
// inventory instead of maintaining parallel product lists.
package productcatalog

// Capability names one behavior exposed by a product integration.
type Capability string

const (
	// CapabilityInteractive marks products with managed interactive sessions.
	CapabilityInteractive Capability = "interactive"
	// CapabilityLane marks products with durable managed lanes.
	CapabilityLane Capability = "lane"
	// CapabilityMCPRelay marks products whose managed sessions expose Agent Sessions MCP.
	CapabilityMCPRelay Capability = "mcp-relay"
	// CapabilityHook marks products with Agent Sessions lifecycle hooks.
	CapabilityHook Capability = "hook"
	// CapabilityArchive marks products with a supported archive operation.
	CapabilityArchive Capability = "archive"
	// CapabilityDynamicPermission marks products whose effective permission can change natively.
	CapabilityDynamicPermission Capability = "dynamic-permission"
)

// ResumeStyle describes the product-native interactive resume argument shape.
type ResumeStyle string

const (
	// ResumeSubcommand produces `resume SESSION_ID`.
	ResumeSubcommand ResumeStyle = "subcommand"
	// ResumeFlag produces `--resume SESSION_ID`.
	ResumeFlag ResumeStyle = "flag"
)

// Descriptor is the authoritative identity and capability inventory for one
// supported native product. It describes stable shared surfaces only; adapter
// code retains all product-specific launch and lifecycle behavior.
type Descriptor struct {
	ID                  string
	Label               string
	NativeExecutable    string
	PeerAlias           string
	LaneAlias           string
	LaneRuntimeRole     string
	LaneManagerRole     string
	LaneCapability      string
	PluginArchivePaths  []string
	Capabilities        []Capability
	ResumeStyle         ResumeStyle
	TranscriptNameIndex bool
}

var descriptors = [...]Descriptor{
	{
		ID: "codex", Label: "Codex", NativeExecutable: "codex", PeerAlias: "codex-peer",
		LaneAlias: "codex-peer-lane", LaneRuntimeRole: "lane", LaneCapability: "codex-lane",
		PluginArchivePaths: []string{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills"},
		Capabilities:       []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay, CapabilityHook, CapabilityArchive},
		ResumeStyle:        ResumeSubcommand,
	},
	{
		ID: "claude", Label: "Claude Code", NativeExecutable: "claude", PeerAlias: "claude-peer",
		LaneAlias: "claude-peer-lane", LaneRuntimeRole: "claude-lane", LaneManagerRole: "claude-lane-manager", LaneCapability: "claude-lane",
		PluginArchivePaths:  []string{".claude-plugin", "claude"},
		Capabilities:        []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay},
		ResumeStyle:         ResumeFlag,
		TranscriptNameIndex: true,
	},
	{
		ID: "grok", Label: "Grok", NativeExecutable: "grok", PeerAlias: "grok-peer",
		LaneAlias: "grok-peer-lane", LaneRuntimeRole: "grok-lane", LaneManagerRole: "grok-lane-manager", LaneCapability: "grok-lane",
		PluginArchivePaths: []string{"grok"},
		Capabilities:       []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission},
		ResumeStyle:        ResumeFlag,
	},
	{
		ID: "qwen", Label: "Qwen Code", NativeExecutable: "qwen", PeerAlias: "qwen-peer",
		LaneAlias: "qwen-peer-lane", LaneRuntimeRole: "qwen-lane", LaneManagerRole: "qwen-lane-manager", LaneCapability: "qwen-lane",
		PluginArchivePaths: []string{"qwen"},
		Capabilities:       []Capability{CapabilityInteractive, CapabilityLane, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission},
		ResumeStyle:        ResumeFlag,
	},
}

// All returns isolated copies of all products in authoritative display order.
func All() []Descriptor {
	products := make([]Descriptor, len(descriptors))
	for index, descriptor := range descriptors {
		products[index] = cloneDescriptor(descriptor)
	}
	return products
}

// ByID resolves an exact canonical lower-case product identifier.
func ByID(id string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.ID == id {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

// ByCommand resolves an exact managed peer or lane command alias. Native
// vendor executable names are deliberately not managed command aliases.
func ByCommand(command string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.PeerAlias == command || descriptor.LaneAlias == command {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

// ByLaneCapability resolves an exact lane capability advertised through federation.
func ByLaneCapability(capability string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.LaneCapability == capability {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

// Has reports whether the descriptor declares one capability.
func (d Descriptor) Has(capability Capability) bool {
	for _, candidate := range d.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

// ResumeArguments returns the product-native interactive resume syntax.
func (d Descriptor) ResumeArguments(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	if d.ResumeStyle == ResumeFlag {
		return []string{"--resume", sessionID}
	}
	return []string{"resume", sessionID}
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.PluginArchivePaths = append([]string(nil), descriptor.PluginArchivePaths...)
	descriptor.Capabilities = append([]Capability(nil), descriptor.Capabilities...)
	return descriptor
}
