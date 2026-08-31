package releaseevidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaselineFunctionalLedgerMapsEveryAcceptanceCell(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	manifest := loadAcceptanceTestManifest(t, repositoryRoot)
	cells, err := manifest.ValidateAndExpand(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	portMap := loadBaselinePortMapForTest(t, repositoryRoot)
	knownEntries := make(map[string]bool, len(portMap.Entries))
	for _, entry := range portMap.Entries {
		knownEntries[entry.ID] = true
	}
	path := filepath.Join(repositoryRoot, "specs", "002-unified-user-daemon", "evidence", "baseline-functional-cells.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document := string(body)
	if strings.Contains(document, "UNMAPPED") {
		t.Fatal("baseline functional ledger contains an unmapped cell")
	}
	section, err := markdownSection(document, "Per-cell baseline ledger")
	if err != nil {
		t.Fatal(err)
	}
	rowCount := 0
	for _, line := range strings.Split(section, "\n") {
		columns := markdownColumns(line)
		if len(columns) != 5 || columns[0] == "Cell" || strings.HasPrefix(columns[0], "---") {
			continue
		}
		rowCount++
		for _, entryID := range strings.Split(columns[1], ",") {
			entryID = strings.TrimSpace(entryID)
			if !knownEntries[entryID] {
				t.Fatalf("cell %s references unknown baseline port-map entry %s", columns[0], entryID)
			}
		}
		if !strings.Contains(document, "**"+columns[2]) || !strings.Contains(document, "**"+columns[3]+"**") {
			t.Fatalf("cell %s references undefined recipe or artifact contract", columns[0])
		}
		if strings.TrimSpace(columns[4]) == "" {
			t.Fatalf("cell %s has an empty baseline assertion", columns[0])
		}
	}
	if rowCount != len(cells) {
		t.Fatalf("baseline functional ledger rows = %d, want %d", rowCount, len(cells))
	}
	for _, cell := range cells {
		if count := markdownTableCellCount(section, cell.ID); count != 1 {
			t.Fatalf("baseline functional ledger cell %s occurs %d times, want 1", cell.ID, count)
		}
	}
}

func TestBaselineFunctionalLedgerRecordsConvergenceAndEveryTopologyDelta(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	manifest := loadAcceptanceTestManifest(t, repositoryRoot)
	path := filepath.Join(repositoryRoot, "specs", "002-unified-user-daemon", "evidence", "baseline-functional-cells.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document := string(body)
	convergence, err := markdownSection(document, "Existing convergence predicates and deadlines")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"U-03", "C-16", "CL-06", "G-12", "G-18", "Q-08", "L-16", "L-25", "L-29", "X-05"} {
		if !strings.Contains(convergence, id) {
			t.Fatalf("convergence ledger does not name restart/reconnect cell %s", id)
		}
	}
	if !strings.Contains(convergence, "deadline") && !strings.Contains(convergence, "Timeout") {
		t.Fatal("convergence ledger does not record bounded deadlines")
	}
	topology, err := markdownSection(document, "Reviewed topology deltas")
	if err != nil {
		t.Fatal(err)
	}
	for id, delta := range manifest.TopologyDeltas {
		if count := markdownTableCellCount(topology, id); count != 1 {
			t.Fatalf("topology delta %s occurs %d times, want 1", id, count)
		}
		for _, value := range []string{delta.BaselineObservation, delta.TargetObservation, delta.PreservedInvariant} {
			if !strings.Contains(topology, value) {
				t.Fatalf("topology delta %s is missing exact contract text %q", id, value)
			}
		}
	}
}

func markdownColumns(line string) []string {
	if !strings.HasPrefix(strings.TrimSpace(line), "|") {
		return nil
	}
	parts := strings.Split(line, "|")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
