package launcher

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/antst/sessionbus/internal/productcatalog"
)

// TestPeerResumeBelongsEntirelyToTheProduct pins the owner's permanent rule:
// Agent Sessions may translate the wrapper's resume spelling, but it must not
// list, title-match across sessions, or choose the selector. The product
// receives the bytes verbatim and owns every name, ID, picker, and error; a
// launcher may classify those same bytes only to verify the identity that the
// product reports after making its choice.
func TestPeerResumeBelongsEntirelyToTheProduct(t *testing.T) {
	want := map[string][]string{
		"codex": {"resume", "opaque duplicate name"}, "claude": {"--resume", "opaque duplicate name"},
		"grok": {"--resume", "opaque duplicate name"}, "qwen": {"--resume", "opaque duplicate name"},
		"opencode": {"--session", "opaque duplicate name"}, "kilo": {"--session", "opaque duplicate name"},
		"pi": {"--resume", "opaque duplicate name"}, "omp": {"--resume", "opaque duplicate name"},
	}
	for product, expected := range want {
		descriptor, ok := productcatalog.ByID(product)
		if !ok {
			t.Fatalf("missing descriptor %s", product)
		}
		got, err := projectNativeArgumentRules(descriptor, []string{"--resume", "opaque duplicate name"})
		if err != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s resume projection = %#v, %v; want %#v", product, got, err, expected)
		}
		bare, err := projectNativeArgumentRules(descriptor, []string{"--resume"})
		if err != nil || !reflect.DeepEqual(bare, expected[:1]) {
			t.Fatalf("%s bare resume projection = %#v, %v; want %#v", product, bare, err, expected[:1])
		}
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve launcher package")
	}
	directory := filepath.Dir(file)
	set := token.NewFileSet()
	functions := map[string]*ast.FuncDecl{}
	for _, path := range launcherProductionFiles(t, directory) {
		parsed, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
				functions[function.Name.Name] = function
			}
		}
	}
	forbidden := regexp.MustCompile(`(?i)(list.*(thread|session)|(thread|session).*list|choose.*session|resolve.*resume|resume.*resolve)`)
	seen := map[string]bool{}
	var inspect func(string)
	inspect = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		function := functions[name]
		if function == nil || function.Body == nil {
			return
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if forbidden.MatchString(callee.Name) {
				t.Fatalf("peer resume launch graph %s reaches forbidden resolver %s", name, callee.Name)
			}
			inspect(callee.Name)
			return true
		})
	}
	for _, root := range []string{"RunManagedPeer", "RunCodexPeerWithDaemon", "RunClaudePeer", "RunGrokPeer", "RunQwenPeer"} {
		inspect(root)
	}
}

func launcherProductionFiles(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		paths = append(paths, filepath.Join(directory, entry.Name()))
	}
	return paths
}
