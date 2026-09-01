package releaseevidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcceptanceManifestExpandsExactClosedMatrix(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	manifest := loadAcceptanceTestManifest(t, repositoryRoot)
	cells, err := manifest.ValidateAndExpand(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 202 {
		t.Fatalf("expanded acceptance cells = %d, want 202", len(cells))
	}
	seen := make(map[string]bool, len(cells))
	for _, cell := range cells {
		if seen[cell.ID] {
			t.Fatalf("duplicate expanded cell %q", cell.ID)
		}
		seen[cell.ID] = true
		if cell.Family == "" || len(cell.EvidenceTiers) == 0 || len(cell.Platforms) == 0 ||
			cell.AssertionSource.Section == "" || cell.AssertionSource.LocatorTemplate == "" {
			t.Fatalf("cell %s is not fully expanded: %+v", cell.ID, cell)
		}
	}
}

func TestAcceptanceManifestRejectsEveryClosedMatrixDriftClass(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	tests := []struct {
		name   string
		mutate func(*AcceptanceManifest)
		want   string
	}{
		{
			name: "cardinality",
			mutate: func(manifest *AcceptanceManifest) {
				manifest.Families[0].IDs = manifest.Families[0].IDs[:len(manifest.Families[0].IDs)-1]
			},
			want: "cell count",
		},
		{
			name: "duplicate id",
			mutate: func(manifest *AcceptanceManifest) {
				manifest.Families[2].IDs[1] = manifest.Families[2].IDs[0]
			},
			want: "duplicate acceptance cell",
		},
		{
			name: "empty platform override",
			mutate: func(manifest *AcceptanceManifest) {
				manifest.Families[0].CellOverrides[manifest.Families[0].IDs[0]] = AcceptanceCellOverride{Platforms: []string{}}
			},
			want: "platform scope",
		},
		{
			name: "missing assertion section",
			mutate: func(manifest *AcceptanceManifest) {
				manifest.Families[0].AssertionSource.Section = "section that does not exist"
			},
			want: "assertion section",
		},
		{
			name: "prerequisite cycle",
			mutate: func(manifest *AcceptanceManifest) {
				manifest.Families[0].Prerequisites = []string{manifest.Families[0].IDs[0]}
			},
			want: "prerequisite cycle",
		},
		{
			name: "unknown topology delta",
			mutate: func(manifest *AcceptanceManifest) {
				manifest.TopologyDeltas["UNKNOWN"] = TopologyDelta{
					BaselineObservation: "old", TargetObservation: "new", PreservedInvariant: "same",
				}
			},
			want: "unknown acceptance cell",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := loadAcceptanceTestManifest(t, repositoryRoot)
			test.mutate(manifest)
			if _, err := manifest.ValidateAndExpand(repositoryRoot); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestAcceptanceManifestParserRejectsUnknownFields(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	source := filepath.Join(repositoryRoot, "specs", "002-unified-user-daemon", "contracts", "acceptance-matrix.yml")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "acceptance-matrix.yml")
	if err := os.WriteFile(path, append(body, []byte("unknown_contract_field: true\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAcceptanceManifest(path); err == nil || !strings.Contains(err.Error(), "field unknown_contract_field") {
		t.Fatalf("strict parser error = %v", err)
	}
}

func loadAcceptanceTestManifest(t *testing.T, repositoryRoot string) *AcceptanceManifest {
	t.Helper()
	path := filepath.Join(repositoryRoot, "specs", "002-unified-user-daemon", "contracts", "acceptance-matrix.yml")
	manifest, err := LoadAcceptanceManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
