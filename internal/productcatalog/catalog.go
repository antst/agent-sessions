// Package productcatalog defines the sole data-only inventory of native
// products supported by Agent Sessions. Product behavior belongs in runtime
// drivers; all projections consume this inventory instead of parallel lists.
package productcatalog

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

type Capability string

const (
	CapabilityInteractive       Capability = "interactive"
	CapabilityLane              Capability = "lane"
	CapabilityParent            Capability = "parent"
	CapabilityMCPRelay          Capability = "mcp-relay"
	CapabilityHook              Capability = "hook"
	CapabilityArchive           Capability = "archive"
	CapabilityDynamicPermission Capability = "dynamic-permission"
)

type SupportState string

const (
	SupportHidden       SupportState = "hidden"
	SupportExperimental SupportState = "experimental"
	SupportGeneral      SupportState = "general"
)

type VersionPolicy string

const (
	VersionExact   VersionPolicy = "exact"
	VersionMinimum VersionPolicy = "minimum"
)

type TupleMember struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Compatibility struct {
	Policy         VersionPolicy `json:"policy"`
	PackageManager string        `json:"package_manager,omitempty"`
	TupleMembers   []TupleMember `json:"tuple_members,omitempty"`
}

type ResumeStyle string

const (
	ResumeSubcommand ResumeStyle = "subcommand"
	ResumeFlag       ResumeStyle = "flag"
)

// Descriptor is the authoritative stable inventory for one product.
// Transitional legacy fields remain until all original-four consumers move to
// the explicit runtime registry during central composition.
type Descriptor struct {
	ID                     string
	Label                  string
	NativeExecutable       string
	PeerAlias              string
	LaneAlias              string
	LaneRuntimeRole        string
	LaneManagerRole        string
	LaneCapability         string
	PluginArchivePaths     []string
	Capabilities           []Capability
	ResumeStyle            ResumeStyle
	TranscriptNameIndex    bool
	SupportState           SupportState
	TestedVersion          string
	Compatibility          Compatibility
	PeerTransport          string
	MessageTransport       string
	LaneTransport          string
	ConnectorAttesterKey   string
	DoctorProbeKey         string
	PermissionProfileKey   string
	InstallRoot            string
	RequiredDoctorFeatures []string
	FederationCapabilities []string
}

var descriptors = [...]Descriptor{
	baselineDescriptor("codex", "Codex", "codex", "codex-peer", "codex-peer-lane", "lane", "", "codex-lane", []string{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay, CapabilityHook, CapabilityArchive}, ResumeSubcommand, false, "0.151.0"),
	baselineDescriptor("claude", "Claude Code", "claude", "claude-peer", "claude-peer-lane", "claude-lane", "claude-lane-manager", "claude-lane", []string{".claude-plugin", "claude"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay}, ResumeFlag, true, "2.1.252"),
	baselineDescriptor("grok", "Grok", "grok", "grok-peer", "grok-peer-lane", "grok-lane", "grok-lane-manager", "grok-lane", []string{"grok"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission}, ResumeFlag, false, "1.0.5"),
	baselineDescriptor("qwen", "Qwen Code", "qwen", "qwen-peer", "qwen-peer-lane", "qwen-lane", "qwen-lane-manager", "qwen-lane", []string{"qwen"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission}, ResumeFlag, false, "0.22.0"),
}

func baselineDescriptor(id, label, executable, peerAlias, laneAlias, runtimeRole, managerRole, laneCapability string, archives []string, capabilities []Capability, resume ResumeStyle, transcriptIndex bool, testedVersion string) Descriptor {
	return Descriptor{
		ID: id, Label: label, NativeExecutable: executable, PeerAlias: peerAlias, LaneAlias: laneAlias,
		LaneRuntimeRole: runtimeRole, LaneManagerRole: managerRole, LaneCapability: laneCapability,
		PluginArchivePaths: archives, Capabilities: capabilities, ResumeStyle: resume,
		TranscriptNameIndex: transcriptIndex, SupportState: SupportGeneral, TestedVersion: testedVersion,
		Compatibility: Compatibility{Policy: VersionMinimum},
		PeerTransport: id + "-peer", MessageTransport: id + "-message", LaneTransport: id + "-lane",
		ConnectorAttesterKey: id + "-parent", DoctorProbeKey: id + "-doctor", PermissionProfileKey: id + "-permission",
		InstallRoot:            "integrations/" + id,
		RequiredDoctorFeatures: []string{"native-cli", "peer", "lane"},
		FederationCapabilities: []string{laneCapability},
	}
}

// ValidateToken validates the bounded token grammar shared by product and
// federation capability namespaces and by runtime registry keys.
func ValidateToken(token string) error {
	if len(token) < 1 || len(token) > 64 {
		return fmt.Errorf("token length %d is outside 1..64", len(token))
	}
	if token[0] == '-' || token[len(token)-1] == '-' {
		return errors.New("token must not start or end with a hyphen")
	}
	if token[0] < 'a' || token[0] > 'z' {
		return errors.New("token must start with a lower-case letter")
	}
	previousHyphen := false
	for _, character := range token {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			if character == '-' && previousHyphen {
				return errors.New("token must not contain consecutive hyphens")
			}
			previousHyphen = character == '-'
			continue
		}
		return fmt.Errorf("token contains invalid character %q", character)
	}
	return nil
}

// ValidateInventory validates one complete injected product inventory.
func ValidateInventory(inventory []Descriptor) error {
	ids, aliases := map[string]bool{}, map[string]bool{}
	federation, installRoots := map[string]bool{}, map[string]bool{}
	for index, descriptor := range inventory {
		if err := validateDescriptor(descriptor); err != nil {
			return fmt.Errorf("descriptor %d (%q): %w", index, descriptor.ID, err)
		}
		if ids[descriptor.ID] {
			return fmt.Errorf("duplicate product id %q", descriptor.ID)
		}
		ids[descriptor.ID] = true
		if installRoots[descriptor.InstallRoot] {
			return fmt.Errorf("duplicate install root %q", descriptor.InstallRoot)
		}
		installRoots[descriptor.InstallRoot] = true
		for _, alias := range []string{descriptor.PeerAlias, descriptor.LaneAlias} {
			if aliases[alias] {
				return fmt.Errorf("duplicate managed alias %q", alias)
			}
			aliases[alias] = true
		}
		for _, capability := range descriptor.FederationCapabilities {
			if federation[capability] {
				return fmt.Errorf("duplicate federation capability %q", capability)
			}
			federation[capability] = true
		}
	}
	return nil
}

func validateDescriptor(descriptor Descriptor) error {
	for _, field := range []struct {
		label string
		token string
	}{
		{label: "id", token: descriptor.ID},
		{label: "peer alias", token: descriptor.PeerAlias},
		{label: "lane alias", token: descriptor.LaneAlias},
		{label: "peer transport", token: descriptor.PeerTransport},
		{label: "message transport", token: descriptor.MessageTransport},
		{label: "lane transport", token: descriptor.LaneTransport},
		{label: "connector attester", token: descriptor.ConnectorAttesterKey},
		{label: "doctor probe", token: descriptor.DoctorProbeKey},
		{label: "permission profile", token: descriptor.PermissionProfileKey},
	} {
		if err := ValidateToken(field.token); err != nil {
			return fmt.Errorf("%s %q: %w", field.label, field.token, err)
		}
	}
	if strings.TrimSpace(descriptor.Label) == "" || strings.TrimSpace(descriptor.NativeExecutable) == "" {
		return errors.New("label and native executable are required")
	}
	if descriptor.SupportState != SupportHidden && descriptor.SupportState != SupportExperimental && descriptor.SupportState != SupportGeneral {
		return fmt.Errorf("invalid support state %q", descriptor.SupportState)
	}
	if strings.TrimSpace(descriptor.TestedVersion) == "" {
		return errors.New("tested version is required")
	}
	if descriptor.Compatibility.Policy != VersionExact && descriptor.Compatibility.Policy != VersionMinimum {
		return fmt.Errorf("invalid compatibility policy %q", descriptor.Compatibility.Policy)
	}
	if descriptor.ResumeStyle != ResumeSubcommand && descriptor.ResumeStyle != ResumeFlag {
		return fmt.Errorf("invalid resume style %q", descriptor.ResumeStyle)
	}
	if len(descriptor.Compatibility.TupleMembers) > 0 {
		if descriptor.Compatibility.Policy != VersionExact || strings.TrimSpace(descriptor.Compatibility.PackageManager) == "" {
			return errors.New("version tuple requires exact policy and package manager")
		}
		seen := map[string]bool{}
		for _, member := range descriptor.Compatibility.TupleMembers {
			if strings.TrimSpace(member.Name) == "" || strings.TrimSpace(member.Version) == "" || seen[member.Name] {
				return errors.New("version tuple members require unique non-empty names and versions")
			}
			seen[member.Name] = true
		}
	}
	if descriptor.InstallRoot != path.Join("integrations", descriptor.ID) {
		return fmt.Errorf("install root %q must be integrations/%s", descriptor.InstallRoot, descriptor.ID)
	}
	if len(descriptor.PluginArchivePaths) == 0 {
		return errors.New("at least one plugin archive path is required")
	}
	archivePaths := map[string]bool{}
	for _, archivePath := range descriptor.PluginArchivePaths {
		if archivePath == "" || path.IsAbs(archivePath) || path.Clean(archivePath) != archivePath || archivePath == "." || archivePath == ".." || strings.HasPrefix(archivePath, "../") || strings.Contains(archivePath, "\\") {
			return fmt.Errorf("plugin archive path %q is not a canonical repository-relative path", archivePath)
		}
		if archivePaths[archivePath] {
			return fmt.Errorf("duplicate plugin archive path %q", archivePath)
		}
		archivePaths[archivePath] = true
	}
	if err := ValidateToken(descriptor.LaneCapability); err != nil {
		return fmt.Errorf("lane capability %q: %w", descriptor.LaneCapability, err)
	}
	if len(descriptor.FederationCapabilities) != 1 || descriptor.FederationCapabilities[0] != descriptor.LaneCapability {
		return errors.New("legacy lane capability must exactly match the sole federation capability")
	}
	seenCapabilities := map[Capability]bool{}
	for _, capability := range descriptor.Capabilities {
		if err := ValidateToken(string(capability)); err != nil {
			return fmt.Errorf("capability %q: %w", capability, err)
		}
		if !knownCapability(capability) {
			return fmt.Errorf("unknown capability %q", capability)
		}
		if seenCapabilities[capability] {
			return fmt.Errorf("duplicate capability %q", capability)
		}
		seenCapabilities[capability] = true
	}
	if err := ValidateToken(descriptor.FederationCapabilities[0]); err != nil {
		return fmt.Errorf("federation capability %q: %w", descriptor.FederationCapabilities[0], err)
	}
	seenFeatures := map[string]bool{}
	for _, feature := range descriptor.RequiredDoctorFeatures {
		if err := ValidateToken(feature); err != nil {
			return fmt.Errorf("doctor feature %q: %w", feature, err)
		}
		if seenFeatures[feature] {
			return fmt.Errorf("duplicate doctor feature %q", feature)
		}
		seenFeatures[feature] = true
	}
	if descriptor.Has(CapabilityInteractive) && (descriptor.PeerTransport == "" || descriptor.MessageTransport == "") {
		return errors.New("interactive product requires peer and message transports")
	}
	if descriptor.Has(CapabilityLane) && descriptor.LaneTransport == "" {
		return errors.New("lane product requires lane transport")
	}
	if descriptor.Has(CapabilityParent) && descriptor.ConnectorAttesterKey == "" {
		return errors.New("parent product requires connector attester")
	}
	return nil
}

func knownCapability(capability Capability) bool {
	switch capability {
	case CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay,
		CapabilityHook, CapabilityArchive, CapabilityDynamicPermission:
		return true
	default:
		return false
	}
}

func All() []Descriptor { return cloneInventory(descriptors[:]) }

func ByID(id string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.ID == id {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

func ByCommand(command string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.PeerAlias == command || descriptor.LaneAlias == command {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

func ByLaneCapability(capability string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		for _, candidate := range descriptor.FederationCapabilities {
			if candidate == capability {
				return cloneDescriptor(descriptor), true
			}
		}
	}
	return Descriptor{}, false
}

func (d Descriptor) Has(capability Capability) bool {
	for _, candidate := range d.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (d Descriptor) ResumeArguments(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	if d.ResumeStyle == ResumeFlag {
		return []string{"--resume", sessionID}
	}
	return []string{"resume", sessionID}
}

func cloneInventory(inventory []Descriptor) []Descriptor {
	result := make([]Descriptor, len(inventory))
	for index, descriptor := range inventory {
		result[index] = cloneDescriptor(descriptor)
	}
	return result
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.PluginArchivePaths = append([]string(nil), descriptor.PluginArchivePaths...)
	descriptor.Capabilities = append([]Capability(nil), descriptor.Capabilities...)
	descriptor.Compatibility.TupleMembers = append([]TupleMember(nil), descriptor.Compatibility.TupleMembers...)
	descriptor.RequiredDoctorFeatures = append([]string(nil), descriptor.RequiredDoctorFeatures...)
	descriptor.FederationCapabilities = append([]string(nil), descriptor.FederationCapabilities...)
	return descriptor
}

func (d Descriptor) SortedCapabilities() []string {
	result := make([]string, len(d.Capabilities))
	for index, capability := range d.Capabilities {
		result[index] = string(capability)
	}
	sort.Strings(result)
	return result
}
