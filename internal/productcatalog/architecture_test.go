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
	path := filepath.Join(root, "internal", "federator", "product.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	compatibilityCalls := map[string]int{}
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			continue
		}
		for _, specification := range decl.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, expression := range value.Values {
				literal, ok := expression.(*ast.CompositeLit)
				if ok && len(literal.Elts) > 1 {
					t.Fatalf("federator authored a parallel composite inventory at %s", path)
				}
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != "mustProductCapability" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Fatalf("federator compatibility capability is not one fixed baseline literal")
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		compatibilityCalls[value]++
		return true
	})
	wantCompatibility := map[string]int{"codex": 1, "claude": 1, "grok": 1, "qwen": 1}
	if len(compatibilityCalls) != len(wantCompatibility) {
		t.Fatalf("federator compatibility product literals = %v, want frozen baseline %v", compatibilityCalls, wantCompatibility)
	}
	for product, count := range wantCompatibility {
		if compatibilityCalls[product] != count {
			t.Fatalf("federator compatibility product literals = %v, want frozen baseline %v", compatibilityCalls, wantCompatibility)
		}
	}
}

// Existing original-four dispatch literals are a shrinking migration baseline.
// Switch cases, if comparisons, and product-keyed maps/slices are all counted;
// new product dispatch belongs in product packages and the explicit composition
// root rather than another central conditional or parallel inventory.
func TestNoNewProductDispatchSwitches(t *testing.T) {
	want := map[string]int{
		"cmd/agent-sessions/codex_host.go":                4,
		"cmd/agent-sessions/connector.go":                 1,
		"cmd/agent-sessions/hook.go":                      1,
		"cmd/agent-sessions/lane.go":                      29,
		"cmd/agent-sessions/main.go":                      4,
		"cmd/agent-sessions/messaging.go":                 4,
		"internal/bridge/claude_lane.go":                  1,
		"internal/bridge/cleanup.go":                      3,
		"internal/bridge/grok.go":                         1,
		"internal/bridge/grok_lane.go":                    1,
		"internal/bridge/group_context.go":                2,
		"internal/bridge/mcp.go":                          15,
		"internal/bridge/native_lane_acp.go":              2,
		"internal/bridge/qwen_host.go":                    1,
		"internal/bridge/qwen_lane.go":                    1,
		"internal/bridge/qwen_plugin.go":                  2,
		"internal/bridge/runtime.go":                      7,
		"internal/daemon/adapter_claude.go":               1,
		"internal/daemon/adapter_codex.go":                1,
		"internal/daemon/adapter_grok.go":                 1,
		"internal/daemon/adapter_qwen.go":                 1,
		"internal/federator/agent.go":                     1,
		"internal/federator/groups.go":                    2,
		"internal/federator/lane.go":                      6,
		"internal/federator/peer_inspect.go":              2,
		"internal/federator/registration.go":              22,
		"internal/launcher/options.go":                    4,
		"internal/launcher/product.go":                    4,
		"internal/launcher/qwen_peer.go":                  1,
		"internal/procinfo/procinfo.go":                   1,
		"internal/qwenreadiness/native.go":                1,
		"internal/releaseevidence/acceptance_products.go": 8,
		"internal/sessiontools/lane_usage.go":             4,
		"internal/sessiontools/mcp.go":                    8,
		"scripts/realproducts/main.go":                    4,
	}
	root := productCatalogRepositoryRoot(t)
	productIDs := map[string]bool{}
	for _, descriptor := range All() {
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
		"internal/qwenreadiness/",
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
