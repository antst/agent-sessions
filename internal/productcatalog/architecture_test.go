package productcatalog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestFederatorHasNoAuthoredProductInventory(t *testing.T) {
	root := productCatalogRepositoryRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "internal", "federator", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("retired federator package still contains Go files: %v", matches)
	}
}

// Existing original-four dispatch literals are a shrinking migration baseline.
// Switch cases, if comparisons, and product-keyed maps/slices are all counted;
// new product dispatch belongs in product packages and the explicit composition
// root rather than another central conditional or parallel inventory.
func TestNoNewProductDispatchSwitches(t *testing.T) {
	want := map[string]int{
		"cmd/agent-sessions/codex_host.go":                1,
		"cmd/agent-sessions/hook.go":                      1,
		"cmd/agent-sessions/lane.go":                      0,
		"cmd/agent-sessions/main.go":                      4,
		"internal/daemon/adapter_claude.go":               1,
		"internal/daemon/adapter_codex.go":                1,
		"internal/daemon/adapter_grok.go":                 1,
		"internal/daemon/adapter_qwen.go":                 1,
		"internal/launcher/options.go":                    4,
		"internal/launcher/product.go":                    4,
		"internal/procinfo/procinfo.go":                   1,
		"internal/releaseevidence/acceptance_products.go": 8,
		"internal/sessiontools/lane_usage.go":             4,
		"internal/sessiontools/mcp.go":                    6,
		"scripts/realproducts/main.go":                    4,
	}
	root := productCatalogRepositoryRoot(t)
	productIDs := map[string]bool{}
	for _, descriptor := range RuntimeInventory() {
		productIDs[descriptor.ID] = true
	}
	legacyProductIDs := map[string]bool{"codex": true, "claude": true, "grok": true, "qwen": true}
	found := map[string]int{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative := filepath.ToSlash(relativePath(root, path))
		if !guardCentralProductDispatch(relative) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		count := 0
		ast.Inspect(file, func(node ast.Node) bool {
			if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				value, err := strconv.Unquote(literal.Value)
				if err == nil && productIDs[value] && !legacyProductIDs[value] {
					t.Fatalf("new product literal %q in guarded central path %s; use a product package or the explicit composition root", value, relative)
				}
			}
			switch expression := node.(type) {
			case *ast.CaseClause:
				for _, candidate := range expression.List {
					if isProductStringLiteral(candidate, productIDs) {
						count++
					}
				}
			case *ast.BinaryExpr:
				if expression.Op == token.EQL || expression.Op == token.NEQ {
					if isProductStringLiteral(expression.X, productIDs) {
						count++
					}
					if isProductStringLiteral(expression.Y, productIDs) {
						count++
					}
				}
			case *ast.KeyValueExpr:
				if isProductStringLiteral(expression.Key, productIDs) {
					count++
				}
			case *ast.CompositeLit:
				for _, element := range expression.Elts {
					if isProductStringLiteral(element, productIDs) {
						count++
					}
				}
			}
			return true
		})
		if count > 0 {
			found[relative] = count
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var drift []string
	for path, count := range found {
		if want[path] != count {
			drift = append(drift, path+"="+strconv.Itoa(count)+" want "+strconv.Itoa(want[path]))
		}
	}
	for path, count := range want {
		if found[path] != count {
			drift = append(drift, path+"="+strconv.Itoa(found[path])+" want "+strconv.Itoa(count))
		}
	}
	sort.Strings(drift)
	if len(drift) > 0 {
		t.Fatalf("product dispatch switch baseline drifted: %v; new dispatch must use the runtime registry, removed cases shrink this baseline", drift)
	}
}

func TestCodexLaneDispatchCannotReturnToCentralEngine(t *testing.T) {
	root := productCatalogRepositoryRoot(t)
	path := filepath.Join(root, "cmd", "agent-sessions", "lane.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value == "codex" {
			t.Fatalf("Codex lane dispatch returned to %s; compose its LaneDriver at the host root", path)
		}
		return true
	})
}

func TestClaudeLaneDispatchCannotReturnToCentralEngine(t *testing.T) {
	root := productCatalogRepositoryRoot(t)
	path := filepath.Join(root, "cmd", "agent-sessions", "lane.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value == "claude" {
			t.Fatalf("Claude lane dispatch returned to %s; compose its LaneDriver at the host root", path)
		}
		return true
	})
}

func isProductStringLiteral(expression ast.Expr, productIDs map[string]bool) bool {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && productIDs[value]
}

func relativePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return relative
}

func guardCentralProductDispatch(relative string) bool {
	if relative == "cmd/agent-sessions/product_registry.go" || strings.HasPrefix(relative, "internal/products/") || strings.HasPrefix(relative, "scripts/spikes/") {
		return false
	}
	for _, prefix := range []string{
		"cmd/agent-sessions/",
		"internal/bridge/",
		"internal/daemon/",
		"internal/federator/",
		"internal/launcher/",
		"internal/procinfo/",
		"internal/releaseevidence/",
		"internal/sessiontools/",
		"scripts/realproducts/",
	} {
		if strings.HasPrefix(relative, prefix) {
			return true
		}
	}
	return false
}
