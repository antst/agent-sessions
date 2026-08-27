package federator

import (
	"path/filepath"
	"testing"
)

type productDescriptorExpectation struct {
	id                   string
	label                string
	peerExecutable       string
	laneExecutable       string
	laneRuntimeRole      string
	laneManagerRole      string
	federationCapability string
}

var expectedProductDescriptors = []productDescriptorExpectation{
	{
		id: "codex", label: "Codex", peerExecutable: "codex-peer",
		laneExecutable: "codex-peer-lane", laneRuntimeRole: "lane",
		federationCapability: "codex-lane",
	},
	{
		id: "claude", label: "Claude Code", peerExecutable: "claude-peer",
		laneExecutable: "claude-peer-lane", laneRuntimeRole: "claude-lane",
		laneManagerRole: "claude-lane-manager", federationCapability: "claude-lane",
	},
	{
		id: "grok", label: "Grok", peerExecutable: "grok-peer",
		laneExecutable: "grok-peer-lane", laneRuntimeRole: "grok-lane",
		laneManagerRole: "grok-lane-manager", federationCapability: "grok-lane",
	},
	{
		id: "qwen", label: "Qwen Code", peerExecutable: "qwen-peer",
		laneExecutable: "qwen-peer-lane", laneRuntimeRole: "qwen-lane",
		laneManagerRole: "qwen-lane-manager", federationCapability: "qwen-lane",
	},
}

func TestProductDescriptorsMatchFourProductContract(t *testing.T) {
	descriptors := ProductDescriptors()
	if len(descriptors) != len(expectedProductDescriptors) {
		t.Fatalf("product descriptor count = %d, want %d", len(descriptors), len(expectedProductDescriptors))
	}

	for _, want := range expectedProductDescriptors {
		got, ok := ProductByID(want.id)
		if !ok {
			t.Errorf("product descriptor %q is missing", want.id)
			continue
		}
		if got.ID != want.id || got.Label != want.label ||
			got.PeerAlias != want.peerExecutable || got.LaneAlias != want.laneExecutable ||
			got.LaneRuntimeRole != want.laneRuntimeRole || got.LaneManagerRole != want.laneManagerRole ||
			got.LaneCapability != want.federationCapability {
			t.Errorf("product descriptor %q = %+v, want %+v", want.id, got, want)
		}
	}
}

func TestProductDescriptorsHaveUniqueIDsExecutablesRolesAndCapabilities(t *testing.T) {
	descriptors := ProductDescriptors()
	ids := map[string]bool{}
	peerExecutables := map[string]bool{}
	laneExecutables := map[string]bool{}
	laneRuntimeRoles := map[string]bool{}
	laneManagerRoles := map[string]bool{}
	federationCapabilities := map[string]bool{}

	for _, descriptor := range descriptors {
		assertUniqueProductDescriptorValue(t, "id", descriptor.ID, ids)
		assertUniqueProductDescriptorValue(t, "peer executable", descriptor.PeerAlias, peerExecutables)
		assertUniqueProductDescriptorValue(t, "lane executable", descriptor.LaneAlias, laneExecutables)
		assertUniqueProductDescriptorValue(t, "lane runtime role", descriptor.LaneRuntimeRole, laneRuntimeRoles)
		if descriptor.LaneManagerRole != "" {
			assertUniqueProductDescriptorValue(t, "lane manager role", descriptor.LaneManagerRole, laneManagerRoles)
		}
		assertUniqueProductDescriptorValue(t, "federation capability", descriptor.LaneCapability, federationCapabilities)

		if filepath.Base(descriptor.PeerAlias) != descriptor.PeerAlias {
			t.Errorf("product %q peer executable %q is not a basename", descriptor.ID, descriptor.PeerAlias)
		}
		if filepath.Base(descriptor.LaneAlias) != descriptor.LaneAlias {
			t.Errorf("product %q lane executable %q is not a basename", descriptor.ID, descriptor.LaneAlias)
		}
	}
}

func TestProductDescriptorsSupportInteractiveAndLaneResume(t *testing.T) {
	for _, want := range expectedProductDescriptors {
		descriptor, ok := ProductByID(want.id)
		if !ok {
			t.Fatalf("product descriptor %q is missing", want.id)
		}
		for _, kind := range []string{SessionKindInteractive, SessionKindLane} {
			if !descriptor.SupportsResume(kind) {
				t.Errorf("product %q does not support %s resume", want.id, kind)
			}
		}
		if descriptor.SupportsResume("") || descriptor.SupportsResume("unknown") {
			t.Errorf("product %q accepts an unknown resume kind", want.id)
		}
	}

	for _, id := range []string{"", "Codex", "unknown"} {
		if descriptor, ok := ProductByID(id); ok {
			t.Errorf("ProductByID(%q) = %+v, true; want no descriptor", id, descriptor)
		}
	}
}

func assertUniqueProductDescriptorValue(t *testing.T, field, value string, seen map[string]bool) {
	t.Helper()
	if value == "" {
		t.Errorf("product descriptor has an empty %s", field)
		return
	}
	if seen[value] {
		t.Errorf("product descriptor %s %q is duplicated", field, value)
		return
	}
	seen[value] = true
}
