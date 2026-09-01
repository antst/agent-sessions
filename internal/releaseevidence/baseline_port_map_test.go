package releaseevidence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaselinePortMapValidatesCurrentInventoryAndCompleteCellUnion(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	manifest := loadAcceptanceTestManifest(t, repositoryRoot)
	cells, err := manifest.ValidateAndExpand(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	portMap := loadBaselinePortMapForTest(t, repositoryRoot)
	if err := portMap.Validate(cells, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBaselinePortMapRejectsSchemaStageRevisionAndReferenceDrift(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	manifest := loadAcceptanceTestManifest(t, repositoryRoot)
	cells, err := manifest.ValidateAndExpand(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(current, previous *BaselinePortMap)
		want   string
	}{
		{
			name: "unknown acceptance cell",
			mutate: func(current, _ *BaselinePortMap) {
				current.Entries[0].AcceptanceCells = append(current.Entries[0].AcceptanceCells, "UNKNOWN")
			},
			want: "unknown acceptance cell",
		},
		{
			name: "incomplete acceptance union",
			mutate: func(current, _ *BaselinePortMap) {
				current.Entries[0].AcceptanceCells = withoutString(current.Entries[0].AcceptanceCells, "S-08")
			},
			want: "acceptance union is incomplete",
		},
		{
			name: "cumulative transplanted gate",
			mutate: func(current, _ *BaselinePortMap) {
				current.Entries[0].Status = "transplanted"
				current.Entries[0].OldSymbols = []string{"old.Symbol"}
				current.Entries[0].OldTests = []string{"TestOld"}
				current.Entries[0].DeletionPaths = []string{"old/path"}
				current.Entries[0].ReplacementTests = nil
				current.Entries[0].Evidence = nil
			},
			want: "transplanted gate requires nonempty replacement_tests",
		},
		{
			name: "manifest revision not monotonic",
			mutate: func(current, previous *BaselinePortMap) {
				current.ManifestRevision = previous.ManifestRevision
			},
			want: "manifest revision",
		},
		{
			name: "entry status regression",
			mutate: func(current, previous *BaselinePortMap) {
				previous.Entries[0].Status = "shared"
				previous.Entries[0].OldSymbols = []string{"old.Symbol"}
				previous.Entries[0].OldTests = []string{"TestOld"}
				previous.Entries[0].DeletionPaths = []string{"old/path"}
				previous.Entries[0].ReplacementTests = []string{"TestReplacement"}
				previous.Entries[0].Evidence = []string{"evidence/path"}
				current.ManifestRevision = previous.ManifestRevision + 1
			},
			want: "status regressed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := loadBaselinePortMapForTest(t, repositoryRoot)
			previous := loadBaselinePortMapForTest(t, repositoryRoot)
			previous.ManifestRevision = current.ManifestRevision - 1
			test.mutate(current, previous)
			if err := current.Validate(cells, previous); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestBaselinePortMapImplementationAndDeletionGatesFailClosed(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	portMap := loadBaselinePortMapForTest(t, repositoryRoot)
	if err := portMap.RequireImplementation("C-LAUNCH"); err != nil {
		t.Fatalf("transplanted Codex implementation gate error = %v", err)
	}
	blocked := loadBaselinePortMapForTest(t, repositoryRoot)
	blocked.Entries[0].Status = "inventoried"
	if err := blocked.RequireImplementation(blocked.Entries[0].ID); err == nil || !strings.Contains(err.Error(), "requires status transplanted") {
		t.Fatalf("untransplanted implementation gate error = %v", err)
	}
	if err := portMap.RequireDeletion("C-LAUNCH"); err == nil || !strings.Contains(err.Error(), "requires status removable") {
		t.Fatalf("deletion gate error = %v", err)
	}
	if err := portMap.RequireImplementation("UNKNOWN"); err == nil || !strings.Contains(err.Error(), "unknown port-map entry") {
		t.Fatalf("unknown implementation entry error = %v", err)
	}
}

func TestBaselinePortMapParserRejectsHistoryAndUnknownFields(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	source := filepath.Join(repositoryRoot, "specs", "002-unified-user-daemon", "contracts", "baseline-port-map.yml")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{
		"unknown_contract_field: true\n",
		"status_history: [inventory-required]\n",
	} {
		path := filepath.Join(t.TempDir(), "baseline-port-map.yml")
		if err := os.WriteFile(path, append(append([]byte(nil), body...), []byte(extra)...), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBaselinePortMap(path); err == nil || !strings.Contains(err.Error(), "field") {
			t.Fatalf("strict parser error for %q = %v", strings.TrimSpace(extra), err)
		}
	}
}

func TestBaselinePortMapInventoriedReferencesResolveAtPinnedBaseline(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	portMap := loadBaselinePortMapForTest(t, repositoryRoot)
	for _, entry := range portMap.Entries {
		for _, path := range entry.BaselinePaths {
			assertPathExistsAtRevision(t, repositoryRoot, portMap.BaselineCommit, entry.ID, path)
		}
		for _, reference := range append(append([]string(nil), entry.OldSymbols...), entry.OldTests...) {
			assertReferenceExistsAtRevision(t, repositoryRoot, portMap.BaselineCommit, entry.ID, reference)
		}
		for _, path := range entry.DeletionPaths {
			assertPathExistsAtRevision(t, repositoryRoot, portMap.BaselineCommit, entry.ID, path)
		}
		for _, reference := range entry.ReplacementTests {
			assertCurrentReferenceExists(t, repositoryRoot, entry.ID, reference)
		}
		for _, reference := range entry.Evidence {
			path, _, _ := strings.Cut(reference, "#")
			assertCurrentPathExists(t, repositoryRoot, entry.ID, path)
		}
	}
}

func loadBaselinePortMapForTest(t *testing.T, repositoryRoot string) *BaselinePortMap {
	t.Helper()
	path := filepath.Join(repositoryRoot, "specs", "002-unified-user-daemon", "contracts", "baseline-port-map.yml")
	portMap, err := LoadBaselinePortMap(path)
	if err != nil {
		t.Fatal(err)
	}
	return portMap
}

func withoutString(values []string, removed string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != removed {
			result = append(result, value)
		}
	}
	return result
}

func assertRepositoryPath(t *testing.T, entryID, path string) {
	t.Helper()
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != filepath.FromSlash(path) {
		t.Fatalf("port-map entry %s has invalid repository path %q", entryID, path)
	}
}

func assertCurrentPathExists(t *testing.T, repositoryRoot, entryID, path string) {
	t.Helper()
	assertRepositoryPath(t, entryID, path)
	if _, err := os.Lstat(filepath.Join(repositoryRoot, filepath.FromSlash(path))); err != nil {
		t.Fatalf("port-map entry %s current path %q does not exist: %v", entryID, path, err)
	}
}

func assertPathExistsAtRevision(t *testing.T, repositoryRoot, revision, entryID, path string) {
	t.Helper()
	assertRepositoryPath(t, entryID, path)
	command := exec.Command("git", "-C", repositoryRoot, "cat-file", "-e", revision+":"+filepath.ToSlash(path))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("port-map entry %s baseline path %q does not exist at %s: %v: %s", entryID, path, revision, err, strings.TrimSpace(string(output)))
	}
}

func assertCurrentReferenceExists(t *testing.T, repositoryRoot, entryID, reference string) {
	t.Helper()
	separator := strings.LastIndexByte(reference, ':')
	path, symbol := reference, ""
	if separator >= 0 {
		path, symbol = reference[:separator], reference[separator+1:]
	}
	assertCurrentPathExists(t, repositoryRoot, entryID, path)
	if symbol == "" {
		return
	}
	absolute := filepath.Join(repositoryRoot, filepath.FromSlash(path))
	body, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatal(err)
	}
	assertReferenceBody(t, entryID, reference, path, symbol, body)
}

func assertReferenceExistsAtRevision(t *testing.T, repositoryRoot, revision, entryID, reference string) {
	t.Helper()
	separator := strings.LastIndexByte(reference, ':')
	path, symbol := reference, ""
	if separator >= 0 {
		path, symbol = reference[:separator], reference[separator+1:]
	}
	assertPathExistsAtRevision(t, repositoryRoot, revision, entryID, path)
	if symbol == "" {
		return
	}
	body, err := exec.Command("git", "-C", repositoryRoot, "show", revision+":"+filepath.ToSlash(path)).Output()
	if err != nil {
		t.Fatalf("read port-map entry %s baseline reference %q at %s: %v", entryID, reference, revision, err)
	}
	assertReferenceBody(t, entryID, reference, path, symbol, body)
}

func assertReferenceBody(t *testing.T, entryID, reference, path, symbol string, body []byte) {
	t.Helper()
	if filepath.Ext(path) != ".go" {
		for _, line := range strings.Split(string(body), "\n") {
			left, _, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			for _, target := range strings.Fields(left) {
				if target == symbol {
					return
				}
			}
		}
		t.Fatalf("port-map entry %s reference %q does not resolve", entryID, reference)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), path, body, 0)
	if err != nil {
		t.Fatalf("parse %q: %v", path, err)
	}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := function.Name.Name
		if function.Recv != nil && len(function.Recv.List) == 1 {
			name = receiverTypeName(function.Recv.List[0].Type) + "." + name
		}
		if name == symbol {
			return
		}
	}
	t.Fatalf("port-map entry %s reference %q does not resolve", entryID, reference)
}

func receiverTypeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return receiverTypeName(value.X)
	case *ast.IndexExpr:
		return receiverTypeName(value.X)
	case *ast.IndexListExpr:
		return receiverTypeName(value.X)
	default:
		return ""
	}
}
