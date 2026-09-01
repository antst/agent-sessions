package federator

import "github.com/antst/agent-sessions/internal/productcatalog"

// Compatibility names retained for existing federation callers/tests. Their
// values are projected from the sole productcatalog inventory, not authored a
// second time in this package.
var (
	CapabilityCodexLane  = mustProductCapability("codex")
	CapabilityClaudeLane = mustProductCapability("claude")
	CapabilityGrokLane   = mustProductCapability("grok")
	CapabilityQwenLane   = mustProductCapability("qwen")
)

// ProductDescriptor is the legacy federation/launcher view projected from the
// sole data-only product catalog. It remains until consumers move to the
// runtime registry in the central integration phase.
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

func ProductDescriptors() []ProductDescriptor {
	catalog := productcatalog.All()
	result := make([]ProductDescriptor, 0, len(catalog))
	for _, descriptor := range catalog {
		result = append(result, projectProduct(descriptor))
	}
	return result
}

func ProductByID(id string) (ProductDescriptor, bool) {
	descriptor, ok := productcatalog.ByID(id)
	if !ok {
		return ProductDescriptor{}, false
	}
	return projectProduct(descriptor), true
}

func ProductByCapability(capability string) (ProductDescriptor, bool) {
	descriptor, ok := productcatalog.ByLaneCapability(capability)
	if !ok {
		return ProductDescriptor{}, false
	}
	return projectProduct(descriptor), true
}

func (p ProductDescriptor) SupportsResume(kind string) bool {
	return kind == SessionKindInteractive || kind == SessionKindLane
}

func (p ProductDescriptor) ResumeArguments(kind, sessionID string) ([]string, bool) {
	if !p.SupportsResume(kind) || sessionID == "" {
		return nil, false
	}
	if kind == SessionKindLane || !p.interactiveResumeFlag {
		return []string{"resume", sessionID}, true
	}
	return []string{"--resume", sessionID}, true
}

func projectProduct(descriptor productcatalog.Descriptor) ProductDescriptor {
	return ProductDescriptor{
		ID: descriptor.ID, Label: descriptor.Label, PeerExecutable: descriptor.PeerAlias,
		LaneExecutable: descriptor.LaneAlias, LaneRuntimeRole: descriptor.LaneRuntimeRole,
		LaneManagerRole: descriptor.LaneManagerRole, FederationCapability: descriptor.LaneCapability,
		DynamicPermission:     descriptor.Has(productcatalog.CapabilityDynamicPermission),
		TranscriptNameIndex:   descriptor.TranscriptNameIndex,
		interactiveResumeFlag: descriptor.ResumeStyle == productcatalog.ResumeFlag,
	}
}

func mustProductCapability(id string) string {
	descriptor, ok := productcatalog.ByID(id)
	if !ok || descriptor.LaneCapability == "" {
		panic("product catalog lost baseline federation capability for " + id)
	}
	return descriptor.LaneCapability
}
