package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestLaneNativeSessionWritesStayAtReviewedBoundaries(t *testing.T) {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate daemon source directory")
	}
	sourceDir := filepath.Dir(sourceFile)
	files, err := filepath.Glob(filepath.Join(sourceDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}

	// The attachment write is a different durable type. The two lane binders
	// are SetNativeSessionID (validated exact Open) and
	// MarkInjectedAndSetNativeDispatch (exact first native acceptance). The
	// remaining lane writes only copy the already-durable value so callers
	// cannot erase it while advancing another lifecycle fact.
	allowed := map[string]struct{}{
		"attachment.go:SelectNative:attachment":               {},
		"lane.go:SetNativeSessionID:lane":                     {},
		"lane.go:preserveExistingLaneNativeSession:candidate": {},
		"lane_input.go:MarkInjectedAndSetNativeDispatch:lane": {},
		"turn.go:Complete:lane":                               {},
	}
	writes := make([]string, 0, len(allowed))
	for _, path := range files {
		if filepath.Ext(path) != ".go" || filepath.Base(path) == filepath.Base(sourceFile) || strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, left := range assignment.Lhs {
					selector, ok := left.(*ast.SelectorExpr)
					if !ok || selector.Sel.Name != "NativeSessionID" {
						continue
					}
					receiver, ok := selector.X.(*ast.Ident)
					if !ok {
						writes = append(writes, fmt.Sprintf("%s:%s:%T", filepath.Base(path), function.Name.Name, selector.X))
						continue
					}
					writes = append(writes, fmt.Sprintf("%s:%s:%s", filepath.Base(path), function.Name.Name, receiver.Name))
				}
				return true
			})
		}
	}
	sort.Strings(writes)
	if len(writes) != len(allowed) {
		t.Fatalf("review every production NativeSessionID assignment; got %v, allowed %v", writes, sortedNativeSessionWriteKeys(allowed))
	}
	for _, write := range writes {
		if _, ok := allowed[write]; !ok {
			t.Fatalf("unreviewed production NativeSessionID assignment %s; only exact Open, atomic first acceptance, and preservation may write lane authority", write)
		}
	}
}

func sortedNativeSessionWriteKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
