package productruntime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRuntimeHasNoInitRegistrationMechanicsImportsOrReverseCycles(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	allowedRuntimeImports := map[string]bool{
		"github.com/antst/agent-sessions/internal/daemon":         true,
		"github.com/antst/agent-sessions/internal/localtransport": true,
		"github.com/antst/agent-sessions/internal/permissionmode": true,
		"github.com/antst/agent-sessions/internal/procinfo":       true,
		"github.com/antst/agent-sessions/internal/productcatalog": true,
	}
	reverseForbidden := map[string]bool{
		"internal/daemon":         true,
		"internal/localtransport": true,
		"internal/productcatalog": true,
	}
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
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		packageDir := filepath.ToSlash(filepath.Dir(relative))
		if packageDir == "internal/productruntime" {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if ok && function.Recv == nil && function.Name.Name == "init" {
					t.Fatalf("product runtime registers through init in %s", relative)
				}
			}
		}
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return err
			}
			if packageDir == "internal/productruntime" && strings.HasPrefix(importPath, "github.com/antst/agent-sessions/internal/") && !allowedRuntimeImports[importPath] {
				t.Fatalf("product runtime imports mechanics package %q in %s", importPath, relative)
			}
			if reverseForbidden[packageDir] && importPath == "github.com/antst/agent-sessions/internal/productruntime" {
				t.Fatalf("reverse productruntime import in %s", relative)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
