// Command legacy-audit inventories the live external surface of the two
// pre-unification packages without modifying the repository.
//
// It deliberately answers package-boundary questions only. A selector used by
// another production package is a live extraction root. An unreferenced
// exported symbol is not, by itself, proof that its declaring file can be
// deleted: package initializers, shared unexported declarations, interface
// dispatch, and same-package tests still require compilation/gate evidence.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const modulePath = "github.com/antst/agent-sessions"

type reference struct {
	Symbol      string   `json:"symbol"`
	Declaration string   `json:"declaration,omitempty"`
	Production  []string `json:"production_callers,omitempty"`
	Tests       []string `json:"test_callers,omitempty"`
}

type entrypoint struct {
	Symbol          string   `json:"symbol"`
	Declaration     string   `json:"declaration"`
	ProductionCalls []string `json:"production_callers"`
	TestCalls       []string `json:"test_callers"`
}

type packageAudit struct {
	ImportPath             string       `json:"import_path"`
	ProductionFiles        int          `json:"production_files"`
	TestFiles              int          `json:"test_files"`
	ProductionLines        int          `json:"production_lines"`
	TestLines              int          `json:"test_lines"`
	DirectProductionImport []string     `json:"direct_production_importers"`
	DirectTestImport       []string     `json:"direct_test_importers"`
	ExternalReferences     []reference  `json:"external_references"`
	Entrypoints            []entrypoint `json:"legacy_entrypoints"`
}

type deadcodeAudit struct {
	Platform            string         `json:"platform"`
	ToolVersion         string         `json:"tool_version"`
	Command             string         `json:"command"`
	TotalUnreachable    int            `json:"total_unreachable_functions"`
	ByPackage           map[string]int `json:"unreachable_functions_by_package"`
	EntrypointsReported []string       `json:"entrypoints_reported_unreachable"`
	OutputSHA256        string         `json:"output_sha256"`
}

type report struct {
	Contract         string          `json:"contract"`
	BaseCommit       string          `json:"base_commit"`
	Commands         []string        `json:"commands"`
	Packages         []packageAudit  `json:"packages"`
	Deadcode         []deadcodeAudit `json:"deadcode"`
	DuplicateCatalog struct {
		ProductCatalogFile string   `json:"product_catalog_file"`
		FederatorFile      string   `json:"federator_catalog_file"`
		ProductCatalogIDs  []string `json:"product_catalog_ids"`
		FederatorIDs       []string `json:"federator_catalog_ids"`
		IndependentTables  bool     `json:"independent_tables"`
	} `json:"duplicate_product_catalog"`
	StaleForgejo struct {
		File            string   `json:"file"`
		MissingCommands []string `json:"missing_command_directories"`
	} `json:"stale_forgejo_workflow"`
}

type parsedFile struct {
	path    string
	test    bool
	fset    *token.FileSet
	file    *ast.File
	imports map[string]string
}

func main() {
	root := flag.String("root", ".", "repository root")
	deadcodeLinux := flag.String("deadcode-linux", "", "Linux deadcode output generated for the production commands")
	deadcodeDarwin := flag.String("deadcode-darwin", "", "Darwin deadcode output generated for the production commands")
	flag.Parse()

	abs, err := filepath.Abs(*root)
	check(err)
	base := strings.TrimSpace(run(abs, "git", "rev-parse", "HEAD"))
	files, err := parseRepository(abs)
	check(err)

	targets := []string{modulePath + "/internal/bridge", modulePath + "/internal/federator"}
	rep := report{
		Contract:   "six-product-s5-legacy-v1",
		BaseCommit: base,
		Commands: []string{
			"go run ./scripts/spikes/six-product/legacy/audit.go -root . -deadcode-linux <linux-output> -deadcode-darwin <darwin-output>",
			"go install golang.org/x/tools/cmd/deadcode@v0.36.0",
			"GOOS=linux GOARCH=amd64 deadcode ./cmd/agent-sessions ./cmd/agent-sessions-hub",
			"GOOS=darwin GOARCH=arm64 deadcode ./cmd/agent-sessions ./cmd/agent-sessions-hub",
			"go test ./...",
			"scripts/test",
		},
	}
	for _, target := range targets {
		rep.Packages = append(rep.Packages, auditPackage(abs, target, files))
	}
	if *deadcodeLinux != "" {
		rep.Deadcode = append(rep.Deadcode, parseDeadcode(abs, *deadcodeLinux, "linux/amd64"))
	}
	if *deadcodeDarwin != "" {
		rep.Deadcode = append(rep.Deadcode, parseDeadcode(abs, *deadcodeDarwin, "darwin/arm64"))
	}
	rep.DuplicateCatalog.ProductCatalogFile = "internal/productcatalog/catalog.go"
	rep.DuplicateCatalog.FederatorFile = "internal/federator/product.go"
	rep.DuplicateCatalog.ProductCatalogIDs = catalogIDs(files, "internal/productcatalog/catalog.go")
	rep.DuplicateCatalog.FederatorIDs = catalogIDs(files, "internal/federator/product.go")
	rep.DuplicateCatalog.IndependentTables = len(rep.DuplicateCatalog.ProductCatalogIDs) > 0 && len(rep.DuplicateCatalog.FederatorIDs) > 0
	rep.StaleForgejo.File = ".forgejo/workflows/ci.yml"
	rep.StaleForgejo.MissingCommands = staleForgejoCommands(abs)

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	check(encoder.Encode(rep))
}

func parseRepository(root string) ([]parsedFile, error) {
	var result []parsedFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "vendor" || name == "dist" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		imports := make(map[string]string)
		for _, item := range file.Imports {
			importPath, unquoteErr := strconv.Unquote(item.Path.Value)
			if unquoteErr != nil {
				return unquoteErr
			}
			name := filepath.Base(importPath)
			if item.Name != nil {
				name = item.Name.Name
			}
			imports[name] = importPath
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		result = append(result, parsedFile{
			path: filepath.ToSlash(rel), test: strings.HasSuffix(path, "_test.go"),
			fset: fset, file: file, imports: imports,
		})
		return nil
	})
	return result, err
}

func auditPackage(root, target string, files []parsedFile) packageAudit {
	relDir := strings.TrimPrefix(target, modulePath+"/")
	result := packageAudit{ImportPath: target}
	declarations := make(map[string]string)
	refs := make(map[string]*reference)
	prodImporters := make(map[string]struct{})
	testImporters := make(map[string]struct{})

	for _, parsed := range files {
		inTarget := strings.HasPrefix(parsed.path, relDir+"/") && !strings.Contains(strings.TrimPrefix(parsed.path, relDir+"/"), "/")
		if inTarget {
			lines := lineCount(filepath.Join(root, filepath.FromSlash(parsed.path)))
			if parsed.test {
				result.TestFiles++
				result.TestLines += lines
			} else {
				result.ProductionFiles++
				result.ProductionLines += lines
			}
			for _, declaration := range parsed.file.Decls {
				switch item := declaration.(type) {
				case *ast.FuncDecl:
					if item.Name.IsExported() {
						declarations[item.Name.Name] = position(parsed, item.Name.Pos())
					}
				case *ast.GenDecl:
					for _, spec := range item.Specs {
						switch value := spec.(type) {
						case *ast.TypeSpec:
							if value.Name.IsExported() {
								declarations[value.Name.Name] = position(parsed, value.Name.Pos())
							}
						case *ast.ValueSpec:
							for _, name := range value.Names {
								if name.IsExported() {
									declarations[name.Name] = position(parsed, name.Pos())
								}
							}
						}
					}
				}
			}
			continue
		}

		aliases := make(map[string]struct{})
		for alias, importPath := range parsed.imports {
			if importPath == target {
				aliases[alias] = struct{}{}
				if parsed.test {
					testImporters[packageOf(parsed.path)] = struct{}{}
				} else {
					prodImporters[packageOf(parsed.path)] = struct{}{}
				}
			}
		}
		if len(aliases) == 0 {
			continue
		}
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			identifier, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if _, ok = aliases[identifier.Name]; !ok {
				return true
			}
			item := refs[selector.Sel.Name]
			if item == nil {
				item = &reference{Symbol: selector.Sel.Name}
				refs[selector.Sel.Name] = item
			}
			caller := position(parsed, selector.Sel.Pos())
			if parsed.test {
				item.Tests = appendUnique(item.Tests, caller)
			} else {
				item.Production = appendUnique(item.Production, caller)
			}
			return true
		})
	}

	for name, item := range refs {
		item.Declaration = declarations[name]
		sort.Strings(item.Production)
		sort.Strings(item.Tests)
		result.ExternalReferences = append(result.ExternalReferences, *item)
	}
	sort.Slice(result.ExternalReferences, func(i, j int) bool { return result.ExternalReferences[i].Symbol < result.ExternalReferences[j].Symbol })
	result.DirectProductionImport = keys(prodImporters)
	result.DirectTestImport = keys(testImporters)
	for _, symbol := range entrypointsFor(target) {
		entry := entrypoint{Symbol: symbol, Declaration: declarations[symbol], ProductionCalls: []string{}, TestCalls: []string{}}
		if item := refs[symbol]; item != nil {
			entry.ProductionCalls = append(entry.ProductionCalls, item.Production...)
			entry.TestCalls = append(entry.TestCalls, item.Tests...)
		}
		// Same-package references are not selector expressions.
		for _, parsed := range files {
			inTarget := strings.HasPrefix(parsed.path, relDir+"/") && !strings.Contains(strings.TrimPrefix(parsed.path, relDir+"/"), "/")
			if !inTarget {
				continue
			}
			ast.Inspect(parsed.file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != symbol || position(parsed, ident.Pos()) == entry.Declaration {
					return true
				}
				caller := position(parsed, ident.Pos())
				if parsed.test {
					entry.TestCalls = appendUnique(entry.TestCalls, caller)
				} else {
					entry.ProductionCalls = appendUnique(entry.ProductionCalls, caller)
				}
				return true
			})
		}
		sort.Strings(entry.ProductionCalls)
		sort.Strings(entry.TestCalls)
		result.Entrypoints = append(result.Entrypoints, entry)
	}
	return result
}

func parseDeadcode(root, path, platform string) deadcodeAudit {
	body, err := os.ReadFile(path)
	check(err)
	result := deadcodeAudit{
		Platform:    platform,
		ToolVersion: "golang.org/x/tools/cmd/deadcode@v0.36.0",
		Command:     "deadcode ./cmd/agent-sessions ./cmd/agent-sessions-hub",
		ByPackage:   map[string]int{"internal/bridge": 0, "internal/federator": 0},
	}
	result.OutputSHA256 = strings.TrimSpace(run(root, "sha256sum", path))
	if fields := strings.Fields(result.OutputSHA256); len(fields) > 0 {
		result.OutputSHA256 = fields[0]
	}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "unreachable func:") {
			continue
		}
		result.TotalUnreachable++
		for key := range result.ByPackage {
			if strings.HasPrefix(line, key+"/") {
				result.ByPackage[key]++
			}
		}
		if strings.Contains(line, "unreachable func: Main") || strings.Contains(line, "unreachable func: RunAgent") || strings.Contains(line, "unreachable func: RunHub") {
			result.EntrypointsReported = append(result.EntrypointsReported, line)
		}
	}
	check(scanner.Err())
	sort.Strings(result.EntrypointsReported)
	return result
}

func catalogIDs(files []parsedFile, wanted string) []string {
	var ids []string
	for _, parsed := range files {
		if parsed.path != wanted {
			continue
		}
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, element := range literal.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := pair.Key.(*ast.Ident)
				value, valueOK := pair.Value.(*ast.BasicLit)
				if !ok || !valueOK || key.Name != "ID" || value.Kind != token.STRING {
					continue
				}
				text, err := strconv.Unquote(value.Value)
				if err == nil && text != "" {
					ids = appendUnique(ids, text)
				}
			}
			return true
		})
	}
	return ids
}

func staleForgejoCommands(root string) []string {
	path := filepath.Join(root, ".forgejo/workflows/ci.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	known := []string{"agent-session-runtime", "peer", "codex-peer", "claude-peer", "codex-peer-lane", "claude-peer-lane", "grok-peer", "grok-peer-lane", "peer-federator"}
	var missing []string
	for _, name := range known {
		if strings.Contains(string(body), " "+name) || strings.Contains(string(body), "./cmd/"+name) {
			if _, statErr := os.Stat(filepath.Join(root, "cmd", name)); errors.Is(statErr, os.ErrNotExist) {
				missing = append(missing, "cmd/"+name)
			}
		}
	}
	return missing
}

func entrypointsFor(target string) []string {
	if strings.HasSuffix(target, "/bridge") {
		return []string{"Main"}
	}
	return []string{"RunAgent", "RunHub"}
}

func position(file parsedFile, pos token.Pos) string {
	location := file.fset.Position(pos)
	return fmt.Sprintf("%s:%d", file.path, location.Line)
}

func packageOf(path string) string {
	dir := filepath.ToSlash(filepath.Dir(path))
	return modulePath + "/" + strings.TrimPrefix(dir, "./")
}

func lineCount(path string) int {
	body, err := os.ReadFile(path)
	check(err)
	if len(body) == 0 {
		return 0
	}
	return strings.Count(string(body), "\n") + 1
}

func appendUnique(values []string, value string) []string {
	for _, item := range values {
		if item == value {
			return values
		}
	}
	return append(values, value)
}

func keys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for item := range values {
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}

func run(root, name string, args ...string) string {
	command := exec.Command(name, args...)
	command.Dir = root
	body, err := command.CombinedOutput()
	if err != nil {
		panic(fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, body))
	}
	return string(body)
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
