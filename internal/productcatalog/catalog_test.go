package productcatalog

import (
	"slices"
	"testing"
)

func TestCatalogIsTheClosedFourProductTwoBinaryAuthority(t *testing.T) {
	catalog := Catalog()
	if catalog.HubProtocol.Version != 3 {
		t.Fatalf("hub protocol version = %d, want 3", catalog.HubProtocol.Version)
	}

	wantProducts := []ProductDescriptor{
		{ID: "codex", PeerAlias: "codex-peer", LaneAlias: "codex-peer-lane", LaneCapability: "codex-lane", Connector: ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: ".mcp.json", EntryPoint: "scripts/native-entry", Mode: "mcp"}},
		{ID: "claude", PeerAlias: "claude-peer", LaneAlias: "claude-peer-lane", LaneCapability: "claude-lane", Connector: ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: "claude/.mcp.json", EntryPoint: "agent-sessions", Mode: "mcp"}},
		{ID: "grok", PeerAlias: "grok-peer", LaneAlias: "grok-peer-lane", LaneCapability: "grok-lane", Connector: ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: "grok/.mcp.json", EntryPoint: "grok/scripts/native-entry", Mode: "mcp"}},
		{ID: "qwen", PeerAlias: "qwen-peer", LaneAlias: "qwen-peer-lane", LaneCapability: "qwen-lane", Connector: ConnectorDescriptor{Type: "mcp-stdio", ManifestPath: "qwen/mcp.json", EntryPoint: "qwen/scripts/native-entry", Mode: "mcp"}},
	}
	if len(catalog.Products) != len(wantProducts) {
		t.Fatalf("product count = %d, want %d: %+v", len(catalog.Products), len(wantProducts), catalog.Products)
	}
	for index, want := range wantProducts {
		got := catalog.Products[index]
		if got.ID != want.ID || got.PeerAlias != want.PeerAlias || got.LaneAlias != want.LaneAlias ||
			got.LaneCapability != want.LaneCapability {
			t.Errorf("product[%d] = %+v, want core fields %+v", index, got, want)
		}
		if got.Connector != want.Connector {
			t.Errorf("product %s connector = %+v, want %+v", got.ID, got.Connector, want.Connector)
		}
	}

	wantBinaries := []BinaryDescriptor{
		{Name: "agent-sessions", Role: BinaryRoleHost},
		{Name: "agent-sessions-hub", Role: BinaryRoleHub},
	}
	if len(catalog.Binaries) != len(wantBinaries) {
		t.Fatalf("binary inventory = %+v, want %+v", catalog.Binaries, wantBinaries)
	}
	for index, want := range wantBinaries {
		got := catalog.Binaries[index]
		if got.Name != want.Name || got.Role != want.Role {
			t.Errorf("binary[%d] = %+v, want core fields %+v", index, got, want)
		}
	}
	if !slices.Equal(catalog.ReleaseExecutables, []string{"agent-sessions", "agent-sessions-hub"}) {
		t.Errorf("release executable inventory = %q", catalog.ReleaseExecutables)
	}

	wantAliases := []string{
		"peer",
		"codex-peer", "claude-peer", "grok-peer", "qwen-peer",
		"codex-peer-lane", "claude-peer-lane", "grok-peer-lane", "qwen-peer-lane",
	}
	if !slices.Equal(catalog.HostAliases, wantAliases) {
		t.Errorf("host aliases = %q, want %q", catalog.HostAliases, wantAliases)
	}
	for _, alias := range catalog.HostAliases {
		if resolved, ok := catalog.ResolveExecutable(alias); !ok || resolved != "agent-sessions" {
			t.Errorf("host alias %q resolves to %q, %v", alias, resolved, ok)
		}
	}
	if resolved, ok := catalog.ResolveExecutable("agent-sessions-hub"); !ok || resolved != "agent-sessions-hub" {
		t.Errorf("hub executable resolves to %q, %v", resolved, ok)
	}
}

func TestHubProtocolInventoryIsClosedAndCapabilityNeutral(t *testing.T) {
	protocol := Catalog().HubProtocol
	if protocol.MaxFrameBytes != 2*1024*1024 || protocol.MaxLaneInputBytes != 1024*1024 ||
		protocol.MaxAgentFrameBytes != 1024*1024 {
		t.Errorf("protocol bounds = %+v", protocol)
	}
	wantCapabilities := []string{"claude-lane", "codex-lane", "grok-lane", "qwen-lane"}
	if !slices.Equal(protocol.Capabilities, wantCapabilities) {
		t.Errorf("lane capabilities = %q, want %q", protocol.Capabilities, wantCapabilities)
	}
	wantFrames := []string{
		"hello", "hello_ok", "probe", "probe_ok", "snapshot", "roster",
		"group_deliver", "terminal_notice_deliver", "delivery_ack", "delivery_error",
		"lane_exec", "lane_accepted", "lane_cancel", "lane_cancelled", "lane_cancel_refused",
		"lane_archive", "lane_archived", "lane_archive_refused",
		"lane_result", "lane_result_ack", "lane_result_refused",
		"lane_stdout", "lane_stderr", "lane_exit", "lane_error",
		"ping", "pong",
	}
	if !slices.Equal(protocol.FrameTypes, wantFrames) {
		t.Errorf("hub frame inventory = %q, want %q", protocol.FrameTypes, wantFrames)
	}
	if protocol.AgentFrameVersion != 1 || !protocol.RejectLegacyFlatDelivery {
		t.Errorf("neutral frame/legacy delivery contract = %+v", protocol)
	}
	if protocol.ReleaseCoupled {
		t.Error("hub protocol incorrectly couples interoperability to release identity")
	}
}
