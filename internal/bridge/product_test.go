package bridge

import (
	"slices"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestBridgeProductProjectionCoversRuntimeAndMCP(t *testing.T) {
	products := mcpLaneProductIDs()
	for _, descriptor := range federator.ProductDescriptors() {
		projected, ok := bridgeProductByID(descriptor.ID)
		if !ok || projected.descriptor != descriptor {
			t.Fatalf("bridge product projection %s = %+v, %v", descriptor.ID, projected, ok)
		}
		byRole, ok := bridgeProductByLaneRole(descriptor.LaneRuntimeRole)
		if !ok || byRole.descriptor.ID != descriptor.ID {
			t.Fatalf("bridge lane role %q does not resolve to %s", descriptor.LaneRuntimeRole, descriptor.ID)
		}
		if !slices.Contains(products, descriptor.ID) {
			t.Fatalf("MCP product enum is missing %s", descriptor.ID)
		}
	}
	if len(products) != len(federator.ProductDescriptors()) {
		t.Fatalf("MCP product count = %d, want %d", len(products), len(federator.ProductDescriptors()))
	}
}

func TestExistingLaneParsedGroupOptionsAreAdvertisedInHelp(t *testing.T) {
	for product, usage := range map[string]string{
		"codex": laneUsage(), "claude": claudeLaneUsage(), "grok": grokLaneUsage(),
	} {
		for _, option := range []string{"--group GROUP", "--inherit-groups", "--no-inherit-groups"} {
			if !strings.Contains(usage, option) {
				t.Errorf("%s parser option %q is absent from help", product, option)
			}
		}
	}
}
