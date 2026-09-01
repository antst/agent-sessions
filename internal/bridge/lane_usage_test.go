package bridge

import (
	"strings"
	"testing"
)

func TestLaneUsageProjectsEveryEstablishedProductContract(t *testing.T) {
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		usage, ok := LaneUsage(product)
		if !ok || !strings.Contains(usage, product+"-peer-lane") ||
			!strings.Contains(usage, " resume ") || !strings.Contains(usage, " archive ") {
			t.Fatalf("LaneUsage(%q) = %q, %v", product, usage, ok)
		}
	}
	if usage, ok := LaneUsage("unknown"); ok || usage != "" {
		t.Fatalf("unknown LaneUsage = %q, %v", usage, ok)
	}
}
