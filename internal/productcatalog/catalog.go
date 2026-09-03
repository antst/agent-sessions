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
	Policy                VersionPolicy `json:"policy"`
	PackageManager        string        `json:"package_manager,omitempty"`
	PackageManagerVersion string        `json:"package_manager_version,omitempty"`
	TupleMembers          []TupleMember `json:"tuple_members,omitempty"`
}

// NativeRegistration is a secret-free strategy selector. Args are declarative
// strategy parameters, never native argv, and must be unique and sorted.
type NativeRegistration struct {
	Strategy  string   `json:"strategy"`
	Args      []string `json:"args,omitempty"`
	AssetOnly bool     `json:"asset_only,omitempty"`
}

const (
	maxNativeRegistrationArgs  = 32
	maxNativeArgumentRules     = 16
	maxExternalAcceptanceCells = 32
)

// ExternalAcceptanceCell identifies evidence which requires an external
// account or entitlement and therefore cannot be credited by a protocol mock.
type ExternalAcceptanceCell struct {
	ID                 string `json:"id"`
	RequiredForGeneral bool   `json:"required_for_general,omitempty"`
}

// AcceptanceContract describes real-product evidence requirements.
type AcceptanceContract struct {
	RealProductRequired bool                     `json:"real_product_required"`
	ExternalCells       []ExternalAcceptanceCell `json:"external_cells,omitempty"`
}

type ResumeStyle string

const (
	ResumeSubcommand ResumeStyle = "subcommand"
	ResumeFlag       ResumeStyle = "flag"
)

type NativeArgumentRuleKind string

const (
	NativeArgumentTranslation NativeArgumentRuleKind = "translation"
	NativeArgumentHandler     NativeArgumentRuleKind = "handler"
)

type NativeArgumentSurface string

const (
	NativeArgumentPeer NativeArgumentSurface = "peer"
	NativeArgumentLane NativeArgumentSurface = "lane"
)

// NativeArgumentRule is one descriptor-owned wrapper argument operation. A
// translation rewrites one option into native argv; a handler names the
// product-specific operation used only where semantic probes proved that a
// literal translation is insufficient.
type NativeArgumentRule struct {
	Surface     NativeArgumentSurface
	Kind        NativeArgumentRuleKind
	Option      string
	Replacement []string
	ValuePrefix string
	Handler     string
}

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
	DoctorProbeKey         string
	PermissionProfileKey   string
	InstallRoot            string
	RequiredDoctorFeatures []string
	FederationCapabilities []string
	NativeRegistration     NativeRegistration
	NativeValueOptions     []string
	NativeAttachedShort    []string
	NativeToolGrantArgs    []string
	NativeYoloArgs         []string
	NativeArgumentRules    []NativeArgumentRule
	Acceptance             AcceptanceContract
}

// descriptors is the one product inventory used by launch, packaging, live
// reconnect, and operator projections.
var descriptors = [...]Descriptor{
	withNativeArgumentRules(withNativeYolo(
		baselineDescriptor("codex", "Codex", "codex", "codex-peer", "codex-peer-lane", "lane", "", "codex-lane", []string{".agents", ".codex-plugin", "hooks", "scripts", "skills"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay, CapabilityHook, CapabilityArchive}, ResumeSubcommand, false, "0.151.0"),
		"--yolo",
	),
		nativeArgumentTranslation(NativeArgumentPeer, "--resume", "resume"),
		nativeArgumentValueTranslation(NativeArgumentPeer, "--effort", "model_reasoning_effort=", "-c"),
		nativeArgumentValueTranslation(NativeArgumentPeer, "--reasoning-effort", "model_reasoning_effort=", "-c"),
		nativeArgumentTranslation(NativeArgumentLane, "--effort", "--effort"),
		nativeArgumentTranslation(NativeArgumentLane, "--reasoning-effort", "--effort"),
	),
	withNativeArgumentRules(withNativeLaunchPolicy(
		baselineDescriptor("claude", "Claude Code", "claude", "claude-peer", "claude-peer-lane", "claude-lane", "claude-lane-manager", "claude-lane", []string{".claude-plugin", "claude"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay}, ResumeFlag, true, "2.1.252"),
		[]string{"--allowedTools", "mcp__plugin_agent-sessions_agent_sessions__*"},
		[]string{"--dangerously-skip-permissions"},
	),
		nativeArgumentTranslation(NativeArgumentPeer, "--resume", "--resume"),
		nativeArgumentTranslation(NativeArgumentPeer, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentLane, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentPeer, "--effort", "--effort"),
		nativeArgumentTranslation(NativeArgumentPeer, "--reasoning-effort", "--effort"),
		nativeArgumentTranslation(NativeArgumentLane, "--effort", "--effort"),
		nativeArgumentTranslation(NativeArgumentLane, "--reasoning-effort", "--effort"),
	),
	withNativeArgumentRules(withNativeLaunchPolicy(
		baselineDescriptor("grok", "Grok", "grok", "grok-peer", "grok-peer-lane", "grok-lane", "grok-lane-manager", "grok-lane", []string{"grok"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission}, ResumeFlag, false, "1.0.5"),
		[]string{"--allow", "MCPTool(agent_sessions__*)"},
		[]string{"--yolo"},
	),
		nativeArgumentTranslation(NativeArgumentPeer, "--resume", "--resume"),
		nativeArgumentTranslation(NativeArgumentPeer, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentLane, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentPeer, "--effort", "--reasoning-effort"),
		nativeArgumentTranslation(NativeArgumentPeer, "--reasoning-effort", "--reasoning-effort"),
		nativeArgumentTranslation(NativeArgumentLane, "--effort", "--effort"),
		nativeArgumentTranslation(NativeArgumentLane, "--reasoning-effort", "--effort"),
	),
	withNativeArgumentRules(withNativeYolo(
		baselineDescriptor("qwen", "Qwen Code", "qwen", "qwen-peer", "qwen-peer-lane", "qwen-lane", "qwen-lane-manager", "qwen-lane", []string{"qwen"}, []Capability{CapabilityInteractive, CapabilityLane, CapabilityParent, CapabilityMCPRelay, CapabilityArchive, CapabilityDynamicPermission}, ResumeFlag, false, "0.22.0"),
		"--yolo",
	), nativeArgumentTranslation(NativeArgumentPeer, "--resume", "--resume")),
	withNativeArgumentRules(withNativeYolo(newProductDescriptor(
		"opencode", "OpenCode", "opencode", "1.18.25", "opencode-global-plugin",
		"presence", "presence", "opencode-http", "opencode",
		[]Capability{CapabilityInteractive, CapabilityLane, CapabilityArchive, CapabilityDynamicPermission, CapabilityParent},
		[]string{"event-stream", "parent", "plugin-sdk", "prompt-async"},
		[]string{"--log-level", "--port", "--hostname", "--mdns-domain", "--cors", "--model", "-m", "--session", "-s", "--prompt", "--agent"},
		[]string{"-m", "-s"},
	), "--yolo"),
		nativeArgumentTranslation(NativeArgumentPeer, "--resume", "--session"),
		nativeArgumentTranslation(NativeArgumentPeer, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentLane, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentLane, "--effort", "--effort"),
		nativeArgumentTranslation(NativeArgumentLane, "--reasoning-effort", "--effort"),
	),
	withNativeArgumentRules(withNativeYolo(newProductDescriptor(
		"kilo", "Kilo Code", "kilo", "7.5.6", "kilo-global-plugin",
		"presence", "presence", "kilo-http", "kilo",
		[]Capability{CapabilityInteractive, CapabilityLane, CapabilityArchive, CapabilityDynamicPermission, CapabilityParent},
		[]string{"event-stream", "parent", "plugin-sdk", "tui-routing"},
		[]string{"--log-level", "--port", "--hostname", "--mdns-domain", "--cors", "--model", "-m", "--session", "-s", "--prompt", "--agent"},
		[]string{"-m", "-s"},
	), "--yolo"),
		nativeArgumentTranslation(NativeArgumentPeer, "--resume", "--session"),
		nativeArgumentTranslation(NativeArgumentPeer, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentLane, "--agent", "--agent"),
		nativeArgumentTranslation(NativeArgumentLane, "--effort", "--effort"),
		nativeArgumentTranslation(NativeArgumentLane, "--reasoning-effort", "--effort"),
	),
	piDescriptor(),
	ompDescriptor(),
	dshDescriptor(),
}

func newProductDescriptor(
	id, label, executable, testedVersion, strategy, peerTransport, messageTransport, laneTransport, permission string,
	capabilities []Capability, doctorFeatures, nativeValueOptions, nativeAttachedShort []string,
) Descriptor {
	return Descriptor{
		ID: id, Label: label, NativeExecutable: executable,
		PeerAlias: id + "-peer", LaneAlias: id + "-peer-lane", LaneRuntimeRole: id + "-lane",
		LaneManagerRole: id + "-lane-manager", LaneCapability: id + "-lane",
		PluginArchivePaths: []string{"integrations/" + id}, Capabilities: capabilities, ResumeStyle: ResumeFlag,
		SupportState: SupportGeneral, TestedVersion: testedVersion, Compatibility: Compatibility{Policy: VersionExact},
		PeerTransport: peerTransport, MessageTransport: messageTransport, LaneTransport: laneTransport,
		DoctorProbeKey: id + "-doctor", PermissionProfileKey: permission,
		InstallRoot: "integrations/" + id, RequiredDoctorFeatures: doctorFeatures,
		FederationCapabilities: []string{id + "-lane"},
		NativeRegistration:     NativeRegistration{Strategy: strategy},
		NativeValueOptions:     nativeValueOptions,
		NativeAttachedShort:    nativeAttachedShort,
		Acceptance:             AcceptanceContract{RealProductRequired: true},
	}
}

func withNativeYolo(descriptor Descriptor, arguments ...string) Descriptor {
	descriptor.NativeYoloArgs = append([]string(nil), arguments...)
	return descriptor
}

func withNativeLaunchPolicy(descriptor Descriptor, toolGrant, yolo []string) Descriptor {
	descriptor.NativeToolGrantArgs = append([]string(nil), toolGrant...)
	descriptor.NativeYoloArgs = append([]string(nil), yolo...)
	return descriptor
}

func withNativeArgumentRules(descriptor Descriptor, rules ...NativeArgumentRule) Descriptor {
	descriptor.NativeArgumentRules = append([]NativeArgumentRule(nil), rules...)
	return descriptor
}

func nativeArgumentTranslation(surface NativeArgumentSurface, option string, replacement ...string) NativeArgumentRule {
	return NativeArgumentRule{Surface: surface, Kind: NativeArgumentTranslation, Option: option, Replacement: append([]string(nil), replacement...)}
}

func nativeArgumentValueTranslation(surface NativeArgumentSurface, option, valuePrefix string, replacement ...string) NativeArgumentRule {
	rule := nativeArgumentTranslation(surface, option, replacement...)
	rule.ValuePrefix = valuePrefix
	return rule
}

func nativeArgumentHandler(surface NativeArgumentSurface, option, handler string) NativeArgumentRule {
	return NativeArgumentRule{Surface: surface, Kind: NativeArgumentHandler, Option: option, Handler: handler}
}

func dshDescriptor() Descriptor {
	descriptor := Descriptor{
		ID: "dsh", Label: "DeepSeek Harness", NativeExecutable: "dsh",
		LaneAlias: "dsh-peer-lane", LaneRuntimeRole: "dsh-lane",
		LaneManagerRole: "dsh-lane-manager", LaneCapability: "dsh-lane",
		Capabilities: []Capability{CapabilityLane, CapabilityArchive, CapabilityDynamicPermission},
		ResumeStyle:  ResumeFlag, SupportState: SupportGeneral, TestedVersion: "0.1.2-alpha.5",
		Compatibility: Compatibility{
			Policy: VersionExact, PackageManager: "pnpm", PackageManagerVersion: "10.28.1",
			TupleMembers: []TupleMember{
				{Name: "@deepseek-ai/dsh", Version: "0.1.2-alpha.5"},
			},
		},
		LaneTransport: "native-presence-v1", DoctorProbeKey: "dsh-doctor", PermissionProfileKey: "dsh",
		RequiredDoctorFeatures: []string{"exact-tuple", "lane", "native-cli", "native-v1", "pnpm"},
		FederationCapabilities: []string{"dsh-lane"},
		Acceptance:             AcceptanceContract{RealProductRequired: true},
	}
	descriptor.NativeArgumentRules = []NativeArgumentRule{
		nativeArgumentHandler(NativeArgumentLane, "--effort", "dsh-effort-with-model"),
		nativeArgumentHandler(NativeArgumentLane, "--reasoning-effort", "dsh-effort-with-model"),
	}
	return descriptor
}

func piDescriptor() Descriptor {
	descriptor := newProductDescriptor(
		"pi", "Pi Coding Agent", "pi", "0.84.4", "pi-package",
		"presence", "presence", "pi-rpc", "pi",
		[]Capability{CapabilityInteractive, CapabilityLane, CapabilityArchive, CapabilityParent},
		[]string{"extension", "parent", "rpc-ready", "steer"},
		[]string{"--model", "-m", "--extension", "-e", "--session", "--thinking", "--tools", "--exclude-tools"},
		[]string{"-m", "-e"},
	)
	descriptor.NativeToolGrantArgs = []string{"--approve"}
	descriptor.NativeYoloArgs = []string{"--approve"}
	descriptor.NativeArgumentRules = []NativeArgumentRule{
		nativeArgumentTranslation(NativeArgumentPeer, "--resume", "--resume"),
		nativeArgumentTranslation(NativeArgumentPeer, "--effort", "--thinking"),
		nativeArgumentTranslation(NativeArgumentPeer, "--reasoning-effort", "--thinking"),
		nativeArgumentTranslation(NativeArgumentLane, "--effort", "--thinking"),
		nativeArgumentTranslation(NativeArgumentLane, "--reasoning-effort", "--thinking"),
	}
	return descriptor
}

func ompDescriptor() Descriptor {
	descriptor := newProductDescriptor(
		"omp", "Oh My Pi", "omp", "18.0.11", "omp-extension",
		"presence", "presence", "omp-rpc", "omp",
		[]Capability{CapabilityInteractive, CapabilityLane, CapabilityArchive, CapabilityParent},
		[]string{"extension", "parent", "rpc-ready", "steer"},
		[]string{"--model", "-m", "--extension", "-e", "--resume", "--thinking", "--tools", "--exclude-tools", "--approval-mode"},
		[]string{"-m", "-e"},
	)
	descriptor.NativeYoloArgs = []string{"--yolo"}
	descriptor.NativeArgumentRules = []NativeArgumentRule{
		nativeArgumentTranslation(NativeArgumentPeer, "--resume", "--resume"),
		nativeArgumentTranslation(NativeArgumentPeer, "--effort", "--thinking"),
		nativeArgumentTranslation(NativeArgumentPeer, "--reasoning-effort", "--thinking"),
		nativeArgumentTranslation(NativeArgumentLane, "--effort", "--thinking"),
		nativeArgumentTranslation(NativeArgumentLane, "--reasoning-effort", "--thinking"),
	}
	return descriptor
}

func baselineDescriptor(id, label, executable, peerAlias, laneAlias, runtimeRole, managerRole, laneCapability string, archives []string, capabilities []Capability, resume ResumeStyle, transcriptIndex bool, testedVersion string) Descriptor {
	return Descriptor{
		ID: id, Label: label, NativeExecutable: executable, PeerAlias: peerAlias, LaneAlias: laneAlias,
		LaneRuntimeRole: runtimeRole, LaneManagerRole: managerRole, LaneCapability: laneCapability,
		PluginArchivePaths: archives, Capabilities: capabilities, ResumeStyle: resume,
		TranscriptNameIndex: transcriptIndex, SupportState: SupportGeneral, TestedVersion: testedVersion,
		Compatibility: Compatibility{Policy: VersionMinimum},
		PeerTransport: "presence", MessageTransport: "presence", LaneTransport: id + "-lane",
		DoctorProbeKey: id + "-doctor", PermissionProfileKey: id + "-permission",
		InstallRoot:            "integrations/" + id,
		RequiredDoctorFeatures: []string{"native-cli", "peer", "lane"},
		FederationCapabilities: []string{laneCapability},
		NativeRegistration:     NativeRegistration{Strategy: "legacy-native-plugin", Args: []string{id}},
		Acceptance:             AcceptanceContract{RealProductRequired: true},
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

// ValidateVersion validates the one bounded version grammar shared by tested
// product versions, exact tuple members, and exact package-manager versions.
// It deliberately validates identity syntax, not semver ordering or ranges.
func ValidateVersion(version string) error {
	if len(version) < 1 || len(version) > 128 {
		return fmt.Errorf("version length %d is outside 1..128", len(version))
	}
	for index, character := range version {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (index > 0 && (character == '.' || character == '-' || character == '_' || character == '+')) {
			continue
		}
		return fmt.Errorf("version contains invalid character %q", character)
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
		if descriptor.InstallRoot != "" {
			if installRoots[descriptor.InstallRoot] {
				return fmt.Errorf("duplicate install root %q", descriptor.InstallRoot)
			}
			installRoots[descriptor.InstallRoot] = true
		}
		for _, alias := range []string{descriptor.PeerAlias, descriptor.LaneAlias} {
			if alias == "" {
				continue
			}
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
		{label: "lane alias", token: descriptor.LaneAlias},
		{label: "lane transport", token: descriptor.LaneTransport},
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
	if err := ValidateVersion(descriptor.TestedVersion); err != nil {
		return fmt.Errorf("tested version %q: %w", descriptor.TestedVersion, err)
	}
	if descriptor.Compatibility.Policy != VersionExact && descriptor.Compatibility.Policy != VersionMinimum {
		return fmt.Errorf("invalid compatibility policy %q", descriptor.Compatibility.Policy)
	}
	if descriptor.ResumeStyle != ResumeSubcommand && descriptor.ResumeStyle != ResumeFlag {
		return fmt.Errorf("invalid resume style %q", descriptor.ResumeStyle)
	}
	manager := descriptor.Compatibility.PackageManager
	managerVersion := descriptor.Compatibility.PackageManagerVersion
	if (manager == "") != (managerVersion == "") {
		return errors.New("package manager and exact package-manager version must be declared together")
	}
	if manager != "" {
		if descriptor.Compatibility.Policy != VersionExact || len(descriptor.Compatibility.TupleMembers) == 0 {
			return errors.New("package-manager identity is valid only for an exact non-empty tuple")
		}
		if err := ValidateToken(manager); err != nil {
			return fmt.Errorf("package manager %q: %w", manager, err)
		}
		if err := ValidateVersion(managerVersion); err != nil {
			return fmt.Errorf("package-manager version %q: %w", managerVersion, err)
		}
	}
	if len(descriptor.Compatibility.TupleMembers) > 0 {
		if descriptor.Compatibility.Policy != VersionExact || manager == "" {
			return errors.New("version tuple requires exact policy and exact package-manager identity")
		}
		seen := map[string]bool{}
		for _, member := range descriptor.Compatibility.TupleMembers {
			if strings.TrimSpace(member.Name) == "" || seen[member.Name] {
				return errors.New("version tuple members require unique non-empty names and versions")
			}
			if err := ValidateVersion(member.Version); err != nil {
				return fmt.Errorf("tuple member %q version %q: %w", member.Name, member.Version, err)
			}
			seen[member.Name] = true
		}
	}
	if descriptor.InstallRoot == "" {
		if len(descriptor.PluginArchivePaths) != 0 || descriptor.NativeRegistration.Strategy != "" || len(descriptor.NativeRegistration.Args) != 0 || descriptor.NativeRegistration.AssetOnly {
			return errors.New("assetless product cannot declare archives or native registration")
		}
	} else if descriptor.InstallRoot != path.Join("integrations", descriptor.ID) {
		return fmt.Errorf("install root %q must be integrations/%s", descriptor.InstallRoot, descriptor.ID)
	} else if len(descriptor.PluginArchivePaths) == 0 {
		return errors.New("product install root requires at least one plugin archive path")
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
	if descriptor.Has(CapabilityInteractive) {
		for _, field := range []struct {
			label string
			value string
		}{{"peer alias", descriptor.PeerAlias}, {"peer transport", descriptor.PeerTransport}, {"message transport", descriptor.MessageTransport}} {
			if err := ValidateToken(field.value); err != nil {
				return fmt.Errorf("%s %q: %w", field.label, field.value, err)
			}
		}
	} else if descriptor.PeerAlias != "" || descriptor.PeerTransport != "" || descriptor.MessageTransport != "" || descriptor.Has(CapabilityParent) || descriptor.Has(CapabilityMCPRelay) || descriptor.Has(CapabilityHook) {
		return errors.New("lane-only product cannot declare a peer surface")
	}
	if descriptor.Has(CapabilityLane) && descriptor.LaneTransport == "" {
		return errors.New("lane product requires lane transport")
	}
	if descriptor.NativeRegistration.Strategy != "" {
		if err := ValidateToken(descriptor.NativeRegistration.Strategy); err != nil {
			return fmt.Errorf("native registration strategy %q: %w", descriptor.NativeRegistration.Strategy, err)
		}
	} else if descriptor.InstallRoot != "" {
		return errors.New("product install root requires a native registration strategy")
	}
	if len(descriptor.NativeRegistration.Args) > maxNativeRegistrationArgs {
		return fmt.Errorf("native registration has more than %d arguments", maxNativeRegistrationArgs)
	}
	for index, argument := range descriptor.NativeRegistration.Args {
		if err := ValidateToken(argument); err != nil {
			return fmt.Errorf("native registration argument %q: %w", argument, err)
		}
		if index > 0 && descriptor.NativeRegistration.Args[index-1] >= argument {
			return errors.New("native registration arguments must be unique and sorted")
		}
	}
	for _, launchArguments := range []struct {
		label string
		args  []string
	}{
		{label: "native tool grant", args: descriptor.NativeToolGrantArgs},
		{label: "native yolo", args: descriptor.NativeYoloArgs},
	} {
		if len(launchArguments.args) > maxNativeRegistrationArgs {
			return fmt.Errorf("%s has more than %d arguments", launchArguments.label, maxNativeRegistrationArgs)
		}
		for _, argument := range launchArguments.args {
			if strings.TrimSpace(argument) == "" || strings.ContainsRune(argument, 0) {
				return fmt.Errorf("%s contains an invalid argument", launchArguments.label)
			}
		}
	}
	if len(descriptor.NativeArgumentRules) > maxNativeArgumentRules {
		return fmt.Errorf("native argument rules has more than %d entries", maxNativeArgumentRules)
	}
	seenRules := map[string]bool{}
	for _, rule := range descriptor.NativeArgumentRules {
		if rule.Surface != NativeArgumentPeer && rule.Surface != NativeArgumentLane {
			return errors.New("native argument rule contains an invalid surface")
		}
		validOption := strings.HasPrefix(rule.Option, "-") && !strings.ContainsRune(rule.Option, 0)
		if !strings.HasPrefix(rule.Option, "-") {
			validOption = ValidateToken(rule.Option) == nil
		}
		if strings.TrimSpace(rule.Option) != rule.Option || !validOption {
			return errors.New("native argument rule contains an invalid option")
		}
		key := string(rule.Surface) + "\x00" + string(rule.Kind) + "\x00" + rule.Option
		if seenRules[key] {
			return fmt.Errorf("duplicate native argument rule %s for %s", rule.Kind, rule.Option)
		}
		seenRules[key] = true
		switch rule.Kind {
		case NativeArgumentTranslation:
			if rule.Handler != "" || len(rule.Replacement) == 0 {
				return errors.New("native argument translation must have only a replacement")
			}
			if strings.ContainsRune(rule.ValuePrefix, 0) {
				return errors.New("native argument translation contains an invalid value prefix")
			}
			for _, argument := range rule.Replacement {
				if strings.TrimSpace(argument) == "" || strings.ContainsRune(argument, 0) {
					return errors.New("native argument translation contains an invalid replacement")
				}
			}
		case NativeArgumentHandler:
			if len(rule.Replacement) != 0 || rule.ValuePrefix != "" || ValidateToken(rule.Handler) != nil {
				return errors.New("native argument handler must have only a valid handler name")
			}
		default:
			return fmt.Errorf("unknown native argument rule kind %q", rule.Kind)
		}
	}
	seenExternal := map[string]bool{}
	if descriptor.SupportState != SupportHidden && !descriptor.Acceptance.RealProductRequired {
		return errors.New("visible product acceptance requires real-product evidence")
	}
	if len(descriptor.Acceptance.ExternalCells) > maxExternalAcceptanceCells {
		return fmt.Errorf("acceptance has more than %d external cells", maxExternalAcceptanceCells)
	}
	for index, cell := range descriptor.Acceptance.ExternalCells {
		if err := ValidateToken(cell.ID); err != nil {
			return fmt.Errorf("external acceptance cell %q: %w", cell.ID, err)
		}
		if seenExternal[cell.ID] {
			return fmt.Errorf("duplicate external acceptance cell %q", cell.ID)
		}
		if index > 0 && descriptor.Acceptance.ExternalCells[index-1].ID >= cell.ID {
			return errors.New("external acceptance cells must be unique and sorted")
		}
		seenExternal[cell.ID] = true
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

func RuntimeInventory() []Descriptor { return cloneInventory(descriptors[:]) }

func ByID(id string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.ID == id {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

func ByCommand(command string) (Descriptor, bool) {
	if command == "" {
		return Descriptor{}, false
	}
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
	descriptor.NativeRegistration.Args = append([]string(nil), descriptor.NativeRegistration.Args...)
	descriptor.NativeToolGrantArgs = append([]string(nil), descriptor.NativeToolGrantArgs...)
	descriptor.NativeYoloArgs = append([]string(nil), descriptor.NativeYoloArgs...)
	descriptor.NativeArgumentRules = append([]NativeArgumentRule(nil), descriptor.NativeArgumentRules...)
	for index := range descriptor.NativeArgumentRules {
		descriptor.NativeArgumentRules[index].Replacement = append([]string(nil), descriptor.NativeArgumentRules[index].Replacement...)
	}
	descriptor.Acceptance.ExternalCells = append([]ExternalAcceptanceCell(nil), descriptor.Acceptance.ExternalCells...)
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
