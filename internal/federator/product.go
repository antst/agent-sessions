package federator

const (
	// CapabilityCodexLane is the Codex lane host-roster feature identifier.
	CapabilityCodexLane = "codex-lane"
	// CapabilityClaudeLane is the Claude lane host-roster feature identifier.
	CapabilityClaudeLane = "claude-lane"
	// CapabilityGrokLane is the Grok lane host-roster feature identifier.
	CapabilityGrokLane = "grok-lane"
	// CapabilityQwenLane is the Qwen lane host-roster feature identifier.
	CapabilityQwenLane = "qwen-lane"
)

// ProductDescriptor is the authoritative product inventory used by
// federation and projected into launcher/runtime surfaces. Product-specific
// behavior remains behind the referenced roles and executables.
type ProductDescriptor struct {
	ID                    string
	Label                 string
	PeerExecutable        string
	LaneExecutable        string
	LaneRuntimeRole       string
	LaneManagerRole       string
	FederationCapability  string
	DynamicPermission     bool
	TranscriptNameIndex   bool
	interactiveResumeFlag bool
}

var productDescriptors = [...]ProductDescriptor{
	{
		ID: "codex", Label: "Codex", PeerExecutable: "codex-peer",
		LaneExecutable: "codex-peer-lane", LaneRuntimeRole: "lane",
		FederationCapability: CapabilityCodexLane,
	},
	{
		ID: "claude", Label: "Claude Code", PeerExecutable: "claude-peer",
		LaneExecutable: "claude-peer-lane", LaneRuntimeRole: "claude-lane",
		LaneManagerRole: "claude-lane-manager", FederationCapability: CapabilityClaudeLane,
		TranscriptNameIndex:   true,
		interactiveResumeFlag: true,
	},
	{
		ID: "grok", Label: "Grok", PeerExecutable: "grok-peer",
		LaneExecutable: "grok-peer-lane", LaneRuntimeRole: "grok-lane",
		LaneManagerRole: "grok-lane-manager", FederationCapability: CapabilityGrokLane,
		DynamicPermission:     true,
		interactiveResumeFlag: true,
	},
	{
		ID: "qwen", Label: "Qwen Code", PeerExecutable: "qwen-peer",
		LaneExecutable: "qwen-peer-lane", LaneRuntimeRole: "qwen-lane",
		LaneManagerRole: "qwen-lane-manager", FederationCapability: CapabilityQwenLane,
		DynamicPermission:     true,
		interactiveResumeFlag: true,
	},
}

// ProductDescriptors returns an isolated copy of the complete product table.
func ProductDescriptors() []ProductDescriptor {
	return append([]ProductDescriptor(nil), productDescriptors[:]...)
}

// ProductByID resolves one canonical, lower-case product identifier.
func ProductByID(id string) (ProductDescriptor, bool) {
	for _, descriptor := range productDescriptors {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return ProductDescriptor{}, false
}

// ProductByCapability resolves the exact lane capability advertised by a
// product host.
func ProductByCapability(capability string) (ProductDescriptor, bool) {
	for _, descriptor := range productDescriptors {
		if descriptor.FederationCapability == capability {
			return descriptor, true
		}
	}
	return ProductDescriptor{}, false
}

// SupportsResume reports whether the product owns the requested durable
// session kind. All four current products support interactive and lane resume.
func (p ProductDescriptor) SupportsResume(kind string) bool {
	return kind == SessionKindInteractive || kind == SessionKindLane
}

// ResumeArguments returns the product-native wrapper syntax for one exact
// managed session identity.
func (p ProductDescriptor) ResumeArguments(kind, sessionID string) ([]string, bool) {
	if !p.SupportsResume(kind) || sessionID == "" {
		return nil, false
	}
	if kind == SessionKindLane || !p.interactiveResumeFlag {
		return []string{"resume", sessionID}, true
	}
	return []string{"--resume", sessionID}, true
}
