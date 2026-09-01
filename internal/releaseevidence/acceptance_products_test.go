package releaseevidence

import (
	"strings"
	"testing"
)

func TestProductsForAcceptanceCellUsesExactMatrixRoles(t *testing.T) {
	tests := []struct {
		cell AcceptanceCell
		want string
	}{
		{cell: AcceptanceCell{ID: "S-01", Family: "source-package"}, want: ""},
		{cell: AcceptanceCell{ID: "C-01", Family: "codex-interactive"}, want: "codex"},
		{cell: AcceptanceCell{ID: "CL-01", Family: "claude-interactive"}, want: "claude"},
		{cell: AcceptanceCell{ID: "L-01", Family: "lane-lifecycle"}, want: "codex,claude,grok,qwen"},
		{cell: AcceptanceCell{ID: "P-C-CL", Family: "parent-target-composition"}, want: "codex,claude"},
		{cell: AcceptanceCell{ID: "P-CL-CL", Family: "parent-target-composition"}, want: "claude"},
		{cell: AcceptanceCell{ID: "M-CP-GL", Family: "peer-lane-messaging"}, want: "codex,grok"},
		{cell: AcceptanceCell{ID: "M-CLL-QP", Family: "peer-lane-messaging"}, want: "claude,qwen"},
		{cell: AcceptanceCell{ID: "A-Q", Family: "archive-unarchive"}, want: "qwen"},
	}
	for _, test := range tests {
		t.Run(test.cell.ID, func(t *testing.T) {
			if got := strings.Join(ProductsForAcceptanceCell(test.cell), ","); got != test.want {
				t.Fatalf("ProductsForAcceptanceCell(%+v) = %q, want %q", test.cell, got, test.want)
			}
		})
	}
}
