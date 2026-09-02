package sessiontools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This is the exact bridge migration inventory. Product mechanics extracted
// from central code move into their owning product package; every other change
// must shrink the inventory rather than create another bridge dependency.
var frozenLegacyImporters = map[string]bool{
	"cmd/agent-sessions/codex_host.go":  true,
	"cmd/agent-sessions/connector.go":   true,
	"cmd/agent-sessions/federation.go":  true,
	"cmd/agent-sessions/grok_peer.go":   true,
	"cmd/agent-sessions/hook.go":        true,
	"cmd/agent-sessions/lane.go":        true,
	"cmd/agent-sessions/lane_notice.go": true,
	"internal/products/codex/lane.go":   true,
}

func TestNoNewLegacyBridgeOrFederatorImports(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	found := map[string]bool{}
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
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			imports, ok := declaration.(*ast.GenDecl)
			if !ok || imports.Tok != token.IMPORT {
				continue
			}
			for _, specification := range imports.Specs {
				value, err := strconv.Unquote(specification.(*ast.ImportSpec).Path.Value)
				if err != nil {
					return err
				}
				if value == "github.com/antst/agent-sessions/internal/bridge" || value == "github.com/antst/agent-sessions/internal/federator" {
					relative, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					found[filepath.ToSlash(relative)] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var unexpected, stale []string
	for importer := range found {
		if !frozenLegacyImporters[importer] {
			unexpected = append(unexpected, importer)
		}
	}
	for importer := range frozenLegacyImporters {
		if !found[importer] {
			stale = append(stale, importer)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)
	if len(unexpected) > 0 || len(stale) > 0 {
		t.Fatalf("legacy importer allowlist drift: unexpected=%v stale=%v; shrink the allowlist when imports are removed", unexpected, stale)
	}
}
