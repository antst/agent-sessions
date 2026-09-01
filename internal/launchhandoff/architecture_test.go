package launchhandoff

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestProductionSourcesHaveNoPersistenceLoggingJSONOrAmbientMutation keeps the
// handoff's negative security surface executable. Secrets may exist in the
// bounded binary frame and the final exec environment only.
func TestProductionSourcesHaveNoPersistenceLoggingJSONOrAmbientMutation(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Clean(entry.Name())
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		aliases := map[string]string{}
		for _, imported := range file.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "encoding/json" || name == "log" || name == "log/slog" {
				t.Errorf("%s imports forbidden persistence/logging package %q", path, name)
			}
			alias := filepath.Base(name)
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			aliases[alias] = name
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			identifier, ok := selector.X.(*ast.Ident)
			if ok && aliases[identifier.Name] == "os" && selector.Sel.Name == "Setenv" {
				t.Errorf("%s calls forbidden os.Setenv", path)
			}
			return true
		})
	}
}

func TestExportedConsumeAndExecIsBoundToNativeImageReplacement(t *testing.T) {
	client := parseProductionFile(t, "client.go")
	var exported *ast.FuncDecl
	for _, declaration := range client.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "ConsumeAndExec" {
			exported = function
		}
	}
	if exported == nil || exported.Type.Params == nil || len(exported.Type.Params.List) != 4 {
		t.Fatal("ConsumeAndExec must expose only context, endpoint, ticket, and limits")
	}
	bound := false
	ast.Inspect(exported.Body, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == "nativeExec" {
			bound = true
		}
		return true
	})
	if !bound {
		t.Fatal("exported ConsumeAndExec is not bound to nativeExec")
	}

	native := parseProductionFile(t, "native_exec_unix.go")
	seenChdir, seenExec := false, false
	ast.Inspect(native, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok || identifier.Name != "unix" {
			return true
		}
		seenChdir = seenChdir || selector.Sel.Name == "Chdir"
		seenExec = seenExec || selector.Sel.Name == "Exec"
		return true
	})
	if !seenChdir || !seenExec {
		t.Fatalf("native image replacement chdir=%t exec=%t", seenChdir, seenExec)
	}
}

func parseProductionFile(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
