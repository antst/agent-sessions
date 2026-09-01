// Command catalog-spike is a throw-away phase-0 prototype. It proves that a
// staged host binary can be the only authored ten-product inventory while all
// release, install, federation, doctor, and acceptance views are projections.
// It deliberately does not import or modify production catalog code.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
)

const (
	catalogSchema    = "agent-sessions.product-catalog.v1"
	releaseSchema    = "agent-sessions.release-inventory.v1"
	installSchema    = "agent-sessions.install-plan.v1"
	acceptanceSchema = "agent-sessions.acceptance-matrix.v1"
	baseCommit       = "679fe9d3068b6362df867f8d78ce6708c4ce1342"
	// CapabilityParent is intentionally authored in the data catalog rather
	// than inferred from the runtime registry.
	CapabilityParent = "parent"
)

var tokenPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

type tupleMember struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type compatibility struct {
	Policy         string        `json:"policy"`
	PackageManager string        `json:"package_manager,omitempty"`
	Tuple          []tupleMember `json:"tuple,omitempty"`
}

type externalCell struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	BlocksGeneral  bool   `json:"blocks_general"`
	RequirementKey string `json:"requirement_key"`
}

type doctorMetadata struct {
	ProbeKey         string   `json:"probe_key"`
	RequiredFeatures []string `json:"required_features"`
	ExternalGate     string   `json:"external_gate,omitempty"`
}

type acceptanceMetadata struct {
	RealProductRequired bool           `json:"real_product_required"`
	ExternalCells       []externalCell `json:"external_cells,omitempty"`
}

type authorityMetadata struct {
	PeerLaunch          string   `json:"peer_launch"`
	PeerDiscovery       []string `json:"peer_discovery"`
	PeerAttestation     []string `json:"peer_attestation"`
	PeerCredential      string   `json:"peer_credential"`
	PeerRequestHeader   string   `json:"peer_request_header"`
	LaneServerOwnership string   `json:"lane_server_ownership"`
	LaneAuthentication  string   `json:"lane_authentication"`
	LaneSecretLifetime  string   `json:"lane_secret_lifetime"`
}

type descriptor struct {
	ID                     string             `json:"id"`
	Label                  string             `json:"label"`
	SupportState           string             `json:"support_state"`
	NativeExecutable       string             `json:"native_executable"`
	TestedVersion          string             `json:"tested_version"`
	PeerAlias              string             `json:"peer_alias"`
	LaneAlias              string             `json:"lane_alias"`
	Capabilities           []string           `json:"capabilities"`
	FederationCapabilities []string           `json:"federation_capabilities"`
	InstallRoot            string             `json:"install_root"`
	ArchivePaths           []string           `json:"archive_paths"`
	InstallStrategy        string             `json:"install_strategy"`
	PeerTransport          string             `json:"peer_transport"`
	MessageTransport       string             `json:"message_transport"`
	LaneTransport          string             `json:"lane_transport"`
	ConnectorAttester      string             `json:"connector_attester"`
	Doctor                 doctorMetadata     `json:"doctor"`
	PermissionProfile      string             `json:"permission_profile"`
	Compatibility          compatibility      `json:"compatibility"`
	Acceptance             acceptanceMetadata `json:"acceptance"`
	Authority              *authorityMetadata `json:"authority,omitempty"`
}

type catalogProjection struct {
	Schema     string       `json:"schema"`
	BaseCommit string       `json:"base_commit"`
	Products   []descriptor `json:"products"`
}

type payloadProjection struct {
	Product      string   `json:"product"`
	InstallRoot  string   `json:"install_root"`
	ArchivePaths []string `json:"archive_paths"`
}

type releaseInventory struct {
	Schema                 string              `json:"schema"`
	HostAliases            []string            `json:"host_aliases"`
	NativeExecutables      []string            `json:"native_executables"`
	FederationCapabilities []string            `json:"federation_capabilities"`
	PluginPayloads         []payloadProjection `json:"plugin_payloads"`
}

type installAction struct {
	Product                string        `json:"product"`
	SupportState           string        `json:"support_state"`
	Strategy               string        `json:"strategy"`
	NativeExecutable       string        `json:"native_executable"`
	TestedVersion          string        `json:"tested_version"`
	Aliases                []string      `json:"aliases"`
	Payloads               []string      `json:"payloads"`
	Compatibility          compatibility `json:"compatibility"`
	DoctorProbe            string        `json:"doctor_probe"`
	RequiredDoctorFeatures []string      `json:"required_doctor_features"`
	AdvertiseWhenReady     []string      `json:"advertise_when_ready"`
	RollbackReceipt        bool          `json:"rollback_receipt"`
}

type installPlan struct {
	Schema  string          `json:"schema"`
	Actions []installAction `json:"actions"`
}

type acceptanceCell struct {
	Key                 string `json:"key"`
	Product             string `json:"product"`
	Platform            string `json:"platform"`
	Capability          string `json:"capability"`
	Expected            string `json:"expected"`
	RealProductRequired bool   `json:"real_product_required"`
	RequirementKey      string `json:"requirement_key,omitempty"`
}

type acceptanceMatrix struct {
	Schema string           `json:"schema"`
	Cells  []acceptanceCell `json:"cells"`
}

// authoredDescriptors is the only product inventory in this spike. Every
// other product-shaped output below is derived from these records.
var authoredDescriptors = []descriptor{
	{
		ID: "codex", Label: "Codex", SupportState: "general", NativeExecutable: "codex", TestedVersion: "0.151.0",
		PeerAlias: "codex-peer", LaneAlias: "codex-peer-lane",
		Capabilities: []string{"interactive", "lane", "mcp-relay", "hook", "archive", CapabilityParent}, FederationCapabilities: []string{"codex-lane"},
		InstallRoot: "integrations/codex", ArchivePaths: []string{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills"}, InstallStrategy: "codex-marketplace-plugin",
		PeerTransport: "codex-app-server", MessageTransport: "codex-app-server", LaneTransport: "codex-app-server", ConnectorAttester: "codex-native", PermissionProfile: "codex",
		Doctor: doctorMetadata{ProbeKey: "codex", RequiredFeatures: []string{"app-server", "message", "lane", "parent"}}, Compatibility: compatibility{Policy: "exact-evidence"}, Acceptance: realAcceptance(),
	},
	{
		ID: "claude", Label: "Claude Code", SupportState: "general", NativeExecutable: "claude", TestedVersion: "2.1.252",
		PeerAlias: "claude-peer", LaneAlias: "claude-peer-lane",
		Capabilities: []string{"interactive", "lane", "mcp-relay", CapabilityParent}, FederationCapabilities: []string{"claude-lane"},
		InstallRoot: "integrations/claude", ArchivePaths: []string{".claude-plugin", "claude"}, InstallStrategy: "claude-marketplace-plugin",
		PeerTransport: "claude-message-socket", MessageTransport: "claude-message-socket", LaneTransport: "claude-print", ConnectorAttester: "claude-native", PermissionProfile: "claude",
		Doctor: doctorMetadata{ProbeKey: "claude", RequiredFeatures: []string{"message-socket", "resume", "lane", "parent"}}, Compatibility: compatibility{Policy: "exact-evidence"}, Acceptance: realAcceptance(),
	},
	{
		ID: "grok", Label: "Grok", SupportState: "general", NativeExecutable: "grok", TestedVersion: "1.0.5",
		PeerAlias: "grok-peer", LaneAlias: "grok-peer-lane",
		Capabilities: []string{"interactive", "lane", "mcp-relay", "archive", "dynamic-permission", CapabilityParent}, FederationCapabilities: []string{"grok-lane"},
		InstallRoot: "integrations/grok", ArchivePaths: []string{"grok"}, InstallStrategy: "grok-plugin",
		PeerTransport: "grok-native-observer", MessageTransport: "grok-interject", LaneTransport: "grok-acp", ConnectorAttester: "grok-native", PermissionProfile: "grok",
		Doctor: doctorMetadata{ProbeKey: "grok", RequiredFeatures: []string{"interject", "acp", "archive", "parent"}}, Compatibility: compatibility{Policy: "exact-evidence"}, Acceptance: realAcceptance(),
	},
	{
		ID: "qwen", Label: "Qwen Code", SupportState: "general", NativeExecutable: "qwen", TestedVersion: "0.22.0",
		PeerAlias: "qwen-peer", LaneAlias: "qwen-peer-lane",
		Capabilities: []string{"interactive", "lane", "mcp-relay", "archive", "dynamic-permission", CapabilityParent}, FederationCapabilities: []string{"qwen-lane"},
		InstallRoot: "integrations/qwen", ArchivePaths: []string{"qwen"}, InstallStrategy: "qwen-extension",
		PeerTransport: "qwen-input-file", MessageTransport: "qwen-input-file", LaneTransport: "qwen-acp", ConnectorAttester: "qwen-native", PermissionProfile: "qwen",
		Doctor: doctorMetadata{ProbeKey: "qwen", RequiredFeatures: []string{"input-file", "acp", "archive", "parent"}}, Compatibility: compatibility{Policy: "exact-evidence"}, Acceptance: realAcceptance(),
	},
	{
		ID: "opencode", Label: "OpenCode", SupportState: "general", NativeExecutable: "opencode", TestedVersion: "1.18.25",
		PeerAlias: "opencode-peer", LaneAlias: "opencode-peer-lane",
		Capabilities: []string{"interactive", "lane", "archive", "dynamic-permission", CapabilityParent}, FederationCapabilities: []string{"opencode-lane"},
		InstallRoot: "integrations/opencode", ArchivePaths: []string{"integrations/opencode"}, InstallStrategy: "opencode-global-plugin",
		PeerTransport: "component", MessageTransport: "opencode-http", LaneTransport: "opencode-http", ConnectorAttester: "component-native-session", PermissionProfile: "opencode",
		Doctor: doctorMetadata{ProbeKey: "opencode", RequiredFeatures: []string{"plugin-sdk", "prompt-async", "event-stream", "abort", "parent"}}, Compatibility: compatibility{Policy: "exact"}, Acceptance: realAcceptance(),
	},
	{
		ID: "kilo", Label: "Kilo Code", SupportState: "general", NativeExecutable: "kilo", TestedVersion: "7.5.6",
		PeerAlias: "kilo-peer", LaneAlias: "kilo-peer-lane",
		Capabilities: []string{"interactive", "lane", "archive", "dynamic-permission", CapabilityParent}, FederationCapabilities: []string{"kilo-lane"},
		InstallRoot: "integrations/kilo", ArchivePaths: []string{"integrations/kilo"}, InstallStrategy: "kilo-global-plugin",
		PeerTransport: "component", MessageTransport: "kilo-tui-http", LaneTransport: "kilo-http", ConnectorAttester: "component-native-session", PermissionProfile: "kilo",
		Doctor: doctorMetadata{ProbeKey: "kilo", RequiredFeatures: []string{"plugin-sdk", "tui-routing", "event-stream", "abort", "parent"}}, Compatibility: compatibility{Policy: "exact"}, Acceptance: realAcceptance(),
	},
	{
		ID: "pi", Label: "Pi Coding Agent", SupportState: "general", NativeExecutable: "pi", TestedVersion: "0.84.4",
		PeerAlias: "pi-peer", LaneAlias: "pi-peer-lane",
		Capabilities: []string{"interactive", "lane", "archive", CapabilityParent}, FederationCapabilities: []string{"pi-lane"},
		InstallRoot: "integrations/pi", ArchivePaths: []string{"integrations/pi"}, InstallStrategy: "pi-package",
		PeerTransport: "component", MessageTransport: "pi-component", LaneTransport: "pi-rpc", ConnectorAttester: "component-native-session", PermissionProfile: "pi",
		Doctor: doctorMetadata{ProbeKey: "pi", RequiredFeatures: []string{"extension", "rpc-ready", "steer", "agent-settled", "parent"}}, Compatibility: compatibility{Policy: "exact"}, Acceptance: realAcceptance(),
	},
	{
		ID: "omp", Label: "Oh My Pi", SupportState: "general", NativeExecutable: "omp", TestedVersion: "18.0.11",
		PeerAlias: "omp-peer", LaneAlias: "omp-peer-lane",
		Capabilities: []string{"interactive", "lane", "archive", CapabilityParent}, FederationCapabilities: []string{"omp-lane"},
		InstallRoot: "integrations/omp", ArchivePaths: []string{"integrations/omp"}, InstallStrategy: "omp-extension",
		PeerTransport: "component", MessageTransport: "omp-component", LaneTransport: "omp-rpc", ConnectorAttester: "component-native-session", PermissionProfile: "omp",
		Doctor: doctorMetadata{ProbeKey: "omp", RequiredFeatures: []string{"extension", "rpc-ready", "steer", "agent-settled", "parent"}}, Compatibility: compatibility{Policy: "exact"}, Acceptance: realAcceptance(),
	},
	{
		ID: "codebuddy", Label: "CodeBuddy", SupportState: "experimental", NativeExecutable: "codebuddy", TestedVersion: "2.143.0",
		PeerAlias: "codebuddy-peer", LaneAlias: "codebuddy-peer-lane",
		Capabilities: []string{"interactive", "lane", "mcp-relay", "hook", "archive", "dynamic-permission", CapabilityParent}, FederationCapabilities: []string{"codebuddy-lane"},
		InstallRoot: "integrations/codebuddy", ArchivePaths: []string{"integrations/codebuddy"}, InstallStrategy: "codebuddy-wrapper-plugin-mcp",
		PeerTransport: "codebuddy-native-registry-http", MessageTransport: "codebuddy-native-registry-http", LaneTransport: "codebuddy-owned-auth-http", ConnectorAttester: "codebuddy-registry-process", PermissionProfile: "codebuddy",
		Doctor: doctorMetadata{ProbeKey: "codebuddy", RequiredFeatures: []string{
			"native-registry", "session-pid-url", "literal-loopback", "socket-owner-pid", "executable-identity", "process-ancestry",
			"x-codebuddy-request", "session-reply", "owned-server-auth", "memory-only-lane-secret", "job-events", "interrupt", "parent",
		}, ExternalGate: "tencent-model-turn"},
		Compatibility: compatibility{Policy: "exact"}, Acceptance: acceptanceMetadata{RealProductRequired: true, ExternalCells: []externalCell{{ID: "tencent-model-turn", State: "pending-external", BlocksGeneral: true, RequirementKey: "tencent-account"}}},
		Authority: &authorityMetadata{
			PeerLaunch: "managed-wrapper", PeerDiscovery: []string{"native-registry-session-id", "native-registry-pid", "native-registry-url"},
			PeerAttestation: []string{"literal-loopback", "socket-owner-pid", "executable-identity", "process-ancestry"}, PeerCredential: "none", PeerRequestHeader: "X-CodeBuddy-Request: 1",
			LaneServerOwnership: "agent-sessions-owned", LaneAuthentication: "product-password-auth", LaneSecretLifetime: "memory-only",
		},
	},
	{
		ID: "dsh", Label: "DeepSeek Harness", SupportState: "general", NativeExecutable: "dsh", TestedVersion: "0.1.2-alpha.3",
		PeerAlias: "dsh-peer", LaneAlias: "dsh-peer-lane",
		Capabilities: []string{"interactive", "lane", "mcp-relay", "hook", "archive", "dynamic-permission", CapabilityParent}, FederationCapabilities: []string{"dsh-lane"},
		InstallRoot: "integrations/dsh", ArchivePaths: []string{"integrations/dsh"}, InstallStrategy: "dsh-owned-profile",
		PeerTransport: "component", MessageTransport: "dsh-cordis", LaneTransport: "dsh-acp", ConnectorAttester: "dsh-native-session", PermissionProfile: "dsh",
		Doctor:        doctorMetadata{ProbeKey: "dsh", RequiredFeatures: []string{"tuple", "cordis", "acp", "cancel-notification", "native-lease", "parent"}},
		Compatibility: compatibility{Policy: "exact-tuple", PackageManager: "pnpm", Tuple: []tupleMember{{Name: "@deepseek-ai/dsh", Version: "0.1.2-alpha.3"}, {Name: "@deepseek-ai/dsh-acp-app", Version: "0.1.2-alpha.3"}, {Name: "agent-sessions-dsh", Version: "0.1.2-alpha.3"}}}, Acceptance: realAcceptance(),
	},
}

func realAcceptance() acceptanceMetadata { return acceptanceMetadata{RealProductRequired: true} }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-spike:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: catalog-spike validate|catalog|release-inventory|install-plan|acceptance-matrix|digest VIEW|verify VIEW FILE")
	}
	if err := validateDescriptors(authoredDescriptors); err != nil {
		return err
	}
	switch args[0] {
	case "validate":
		if len(args) != 1 {
			return errors.New("validate takes no arguments")
		}
		fmt.Println("valid")
		return nil
	case "catalog", "release-inventory", "install-plan", "acceptance-matrix":
		if len(args) != 1 {
			return fmt.Errorf("%s takes no arguments", args[0])
		}
		body, err := projectionBytes(args[0], authoredDescriptors)
		if err == nil {
			_, err = os.Stdout.Write(body)
		}
		return err
	case "digest":
		if len(args) != 2 {
			return errors.New("digest requires one projection name")
		}
		body, err := projectionBytes(args[1], authoredDescriptors)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		fmt.Println(hex.EncodeToString(sum[:]))
		return nil
	case "verify":
		if len(args) != 3 {
			return errors.New("verify requires projection name and expected JSON path")
		}
		body, err := projectionBytes(args[1], authoredDescriptors)
		if err != nil {
			return err
		}
		expected, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		if !bytes.Equal(body, expected) {
			return fmt.Errorf("%s projection drift", args[1])
		}
		fmt.Println("match")
		return nil
	default:
		return fmt.Errorf("unknown operation %q", args[0])
	}
}

func projectionBytes(name string, input []descriptor) ([]byte, error) {
	products := normalizedDescriptors(input)
	var value any
	switch name {
	case "catalog":
		value = catalogProjection{Schema: catalogSchema, BaseCommit: baseCommit, Products: products}
	case "release-inventory":
		value = deriveReleaseInventory(products)
	case "install-plan":
		value = deriveInstallPlan(products)
	case "acceptance-matrix":
		value = deriveAcceptanceMatrix(products)
	default:
		return nil, fmt.Errorf("unknown projection %q", name)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func normalizedDescriptors(input []descriptor) []descriptor {
	products := append([]descriptor(nil), input...)
	for i := range products {
		products[i].Capabilities = sortedCopy(products[i].Capabilities)
		products[i].FederationCapabilities = sortedCopy(products[i].FederationCapabilities)
		products[i].ArchivePaths = sortedCopy(products[i].ArchivePaths)
		products[i].Doctor.RequiredFeatures = sortedCopy(products[i].Doctor.RequiredFeatures)
		if products[i].Authority != nil {
			authority := *products[i].Authority
			authority.PeerDiscovery = sortedCopy(authority.PeerDiscovery)
			authority.PeerAttestation = sortedCopy(authority.PeerAttestation)
			products[i].Authority = &authority
		}
		products[i].Compatibility.Tuple = append([]tupleMember(nil), products[i].Compatibility.Tuple...)
		sort.Slice(products[i].Compatibility.Tuple, func(a, b int) bool {
			return products[i].Compatibility.Tuple[a].Name < products[i].Compatibility.Tuple[b].Name
		})
		products[i].Acceptance.ExternalCells = append([]externalCell(nil), products[i].Acceptance.ExternalCells...)
		sort.Slice(products[i].Acceptance.ExternalCells, func(a, b int) bool {
			return products[i].Acceptance.ExternalCells[a].ID < products[i].Acceptance.ExternalCells[b].ID
		})
	}
	sort.Slice(products, func(i, j int) bool { return products[i].ID < products[j].ID })
	return products
}

func deriveReleaseInventory(products []descriptor) releaseInventory {
	result := releaseInventory{Schema: releaseSchema}
	for _, product := range products {
		result.HostAliases = append(result.HostAliases, product.PeerAlias, product.LaneAlias)
		result.NativeExecutables = append(result.NativeExecutables, product.NativeExecutable)
		result.FederationCapabilities = append(result.FederationCapabilities, product.FederationCapabilities...)
		result.PluginPayloads = append(result.PluginPayloads, payloadProjection{Product: product.ID, InstallRoot: product.InstallRoot, ArchivePaths: sortedCopy(product.ArchivePaths)})
	}
	sort.Strings(result.HostAliases)
	sort.Strings(result.NativeExecutables)
	sort.Strings(result.FederationCapabilities)
	return result
}

func deriveInstallPlan(products []descriptor) installPlan {
	result := installPlan{Schema: installSchema}
	for _, product := range products {
		result.Actions = append(result.Actions, installAction{
			Product: product.ID, SupportState: product.SupportState, Strategy: product.InstallStrategy,
			NativeExecutable: product.NativeExecutable, TestedVersion: product.TestedVersion,
			Aliases: sortedCopy([]string{product.PeerAlias, product.LaneAlias}), Payloads: sortedCopy(product.ArchivePaths),
			Compatibility: product.Compatibility, DoctorProbe: product.Doctor.ProbeKey,
			RequiredDoctorFeatures: sortedCopy(product.Doctor.RequiredFeatures), AdvertiseWhenReady: sortedCopy(product.FederationCapabilities), RollbackReceipt: true,
		})
	}
	return result
}

func deriveAcceptanceMatrix(products []descriptor) acceptanceMatrix {
	result := acceptanceMatrix{Schema: acceptanceSchema}
	for _, product := range products {
		for _, platform := range []string{"darwin", "linux"} {
			for _, capability := range product.Capabilities {
				result.Cells = append(result.Cells, acceptanceCell{Key: product.ID + "/" + platform + "/" + capability, Product: product.ID, Platform: platform, Capability: capability, Expected: "required", RealProductRequired: product.Acceptance.RealProductRequired})
			}
			for _, external := range product.Acceptance.ExternalCells {
				result.Cells = append(result.Cells, acceptanceCell{Key: product.ID + "/" + platform + "/" + external.ID, Product: product.ID, Platform: platform, Capability: external.ID, Expected: external.State, RealProductRequired: true, RequirementKey: external.RequirementKey})
			}
		}
	}
	sort.Slice(result.Cells, func(i, j int) bool { return result.Cells[i].Key < result.Cells[j].Key })
	return result
}

func validateDescriptors(input []descriptor) error {
	if len(input) != 10 {
		return fmt.Errorf("product count = %d, want 10", len(input))
	}
	ids, aliases, federation := map[string]bool{}, map[string]bool{}, map[string]bool{}
	knownCapabilities := map[string]bool{"interactive": true, "lane": true, "mcp-relay": true, "hook": true, "archive": true, "dynamic-permission": true, CapabilityParent: true}
	for _, product := range input {
		if err := validateToken("product", product.ID); err != nil {
			return err
		}
		if ids[product.ID] {
			return fmt.Errorf("duplicate product %q", product.ID)
		}
		ids[product.ID] = true
		if product.Label == "" || product.NativeExecutable == "" || product.TestedVersion == "" {
			return fmt.Errorf("%s has incomplete identity/version", product.ID)
		}
		if product.SupportState != "general" && product.SupportState != "experimental" && product.SupportState != "hidden" {
			return fmt.Errorf("%s has invalid support state %q", product.ID, product.SupportState)
		}
		for _, alias := range []string{product.PeerAlias, product.LaneAlias} {
			if err := validateToken("alias", alias); err != nil {
				return err
			}
			if aliases[alias] {
				return fmt.Errorf("duplicate alias %q", alias)
			}
			aliases[alias] = true
		}
		if product.PeerAlias != product.ID+"-peer" || product.LaneAlias != product.ID+"-peer-lane" {
			return fmt.Errorf("%s aliases do not derive from id", product.ID)
		}
		if path.Clean(product.InstallRoot) != "integrations/"+product.ID {
			return fmt.Errorf("%s install root = %q", product.ID, product.InstallRoot)
		}
		if len(product.ArchivePaths) == 0 || product.InstallStrategy == "" {
			return fmt.Errorf("%s lacks payload/strategy", product.ID)
		}
		if err := validateToken("install-strategy", product.InstallStrategy); err != nil {
			return err
		}
		if product.PeerTransport == "" || product.MessageTransport == "" || product.LaneTransport == "" || product.ConnectorAttester == "" || product.PermissionProfile == "" {
			return fmt.Errorf("%s lacks runtime strategy key", product.ID)
		}
		if product.Doctor.ProbeKey == "" || len(product.Doctor.RequiredFeatures) == 0 {
			return fmt.Errorf("%s lacks doctor metadata", product.ID)
		}
		seenCapability := map[string]bool{}
		for _, capability := range product.Capabilities {
			if err := validateToken("capability", capability); err != nil {
				return err
			}
			if !knownCapabilities[capability] {
				return fmt.Errorf("%s has unknown capability %q", product.ID, capability)
			}
			if seenCapability[capability] {
				return fmt.Errorf("%s repeats capability %q", product.ID, capability)
			}
			seenCapability[capability] = true
		}
		if !seenCapability["interactive"] || !seenCapability["lane"] || !seenCapability[CapabilityParent] {
			return fmt.Errorf("%s lacks explicit symmetric capability", product.ID)
		}
		for _, capability := range product.FederationCapabilities {
			if err := validateToken("federation-capability", capability); err != nil {
				return err
			}
			if federation[capability] {
				return fmt.Errorf("duplicate federation capability %q", capability)
			}
			federation[capability] = true
		}
		if len(product.FederationCapabilities) != 1 || product.FederationCapabilities[0] != product.ID+"-lane" {
			return fmt.Errorf("%s federation capability does not derive from id", product.ID)
		}
		for _, external := range product.Acceptance.ExternalCells {
			if err := validateToken("acceptance-capability", external.ID); err != nil {
				return err
			}
			if external.State == "pending-external" && external.BlocksGeneral && product.SupportState == "general" {
				return fmt.Errorf("%s is general with a blocking pending external cell", product.ID)
			}
		}
	}
	dsh := findDescriptor(input, "dsh")
	if dsh == nil || dsh.Compatibility.Policy != "exact-tuple" || dsh.Compatibility.PackageManager != "pnpm" || len(dsh.Compatibility.Tuple) != 3 {
		return errors.New("dsh exact pnpm tuple is incomplete")
	}
	for _, member := range dsh.Compatibility.Tuple {
		if member.Version != dsh.TestedVersion {
			return fmt.Errorf("dsh tuple member %s drifted", member.Name)
		}
	}
	codebuddy := findDescriptor(input, "codebuddy")
	if codebuddy == nil || codebuddy.Authority == nil {
		return errors.New("codebuddy authority metadata is missing")
	}
	authority := codebuddy.Authority
	if authority.PeerLaunch != "managed-wrapper" || authority.PeerCredential != "none" || authority.PeerRequestHeader != "X-CodeBuddy-Request: 1" {
		return errors.New("codebuddy peer authority must use managed wrapper, native no-credential peer, and constant CSRF header")
	}
	if !hasAll(authority.PeerDiscovery, "native-registry-session-id", "native-registry-pid", "native-registry-url") ||
		!hasAll(authority.PeerAttestation, "literal-loopback", "socket-owner-pid", "executable-identity", "process-ancestry") {
		return errors.New("codebuddy peer discovery/attestation metadata is incomplete")
	}
	if authority.LaneServerOwnership != "agent-sessions-owned" || authority.LaneAuthentication != "product-password-auth" || authority.LaneSecretLifetime != "memory-only" {
		return errors.New("codebuddy lane authority must be owned, authenticated, and memory-only")
	}
	return nil
}

func validateToken(namespace, value string) error {
	if len(value) == 0 || len(value) > 64 || !tokenPattern.MatchString(value) {
		return fmt.Errorf("invalid %s token %q", namespace, value)
	}
	return nil
}

func findDescriptor(input []descriptor, id string) *descriptor {
	for i := range input {
		if input[i].ID == id {
			return &input[i]
		}
	}
	return nil
}

func sortedCopy(input []string) []string {
	result := append([]string(nil), input...)
	sort.Strings(result)
	return result
}

func hasAll(input []string, expected ...string) bool {
	seen := make(map[string]bool, len(input))
	for _, value := range input {
		seen[value] = true
	}
	for _, value := range expected {
		if !seen[value] {
			return false
		}
	}
	return true
}
