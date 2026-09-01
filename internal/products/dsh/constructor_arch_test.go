package dsh

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type constructorFunction struct {
	declaration *ast.FuncDecl
	imports     map[string]string
}

var reviewedConstructorFunctions = map[string]bool{
	"NewDrivers": true, "NewCordisGateway": true, "NewDoctorProbe": true,
	"NewPeerDriver": true, "NewMessageDriver": true, "NewParentAttester": true,
	"NewLaneDriver": true, "validateConfiguredProfileManifestShape": true,
	"managedProfileManifestPath": true, "validateManagedDSHHomeShape": true,
	"validateProfileIdentity": true, "temporaryPathLexical": true, "within": true,
	"validCwd": true, "validateComponentSocketShape": true,
}

func analyzeProductionConstructorPurity() ([]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	sources := make(map[string][]byte)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(entry.Name())
		if err != nil {
			return nil, err
		}
		sources[entry.Name()] = body
	}
	return analyzeConstructorPurity(sources, []string{"NewDrivers", "NewPeerDriver", "NewLaneDriver", "NewDoctorProbe"}, reviewedConstructorFunctions)
}

func analyzeConstructorPurity(sources map[string][]byte, roots []string, reviewed map[string]bool) ([]string, error) {
	functions := make(map[string]constructorFunction)
	for filename, body := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), filename, body, 0)
		if err != nil {
			return nil, err
		}
		imports := make(map[string]string)
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			name := filepath.Base(path)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = path
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil {
				continue
			}
			functions[function.Name.Name] = constructorFunction{declaration: function, imports: imports}
		}
	}

	queue := append([]string(nil), roots...)
	visited := make(map[string]bool)
	violations := make([]string, 0)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		function, ok := functions[name]
		if !ok {
			violations = append(violations, "constructor root/helper is missing: "+name)
			continue
		}
		if !reviewed[name] {
			violations = append(violations, "constructor reaches unreviewed local helper: "+name)
		}
		ast.Inspect(function.declaration.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callable := call.Fun.(type) {
			case *ast.Ident:
				if _, local := functions[callable.Name]; local {
					queue = append(queue, callable.Name)
				}
			case *ast.SelectorExpr:
				root := selectorRoot(callable.X)
				importPath := function.imports[root]
				if importPath == "" {
					// Constructors currently require no receiver/interface method
					// calls. Reject them conservatively so a local receiver helper
					// cannot hide live work outside the package-function graph.
					violations = append(violations, fmt.Sprintf("%s reaches unreviewed receiver method %s", name, callable.Sel.Name))
					return true
				}
				if constructorLiveCall(importPath, callable.Sel.Name) {
					violations = append(violations, fmt.Sprintf("%s reaches live call %s.%s", name, root, callable.Sel.Name))
				}
			}
			return true
		})
	}
	sort.Strings(violations)
	return violations, nil
}

func selectorRoot(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return selectorRoot(value.X)
	default:
		return ""
	}
}

func constructorLiveCall(importPath, method string) bool {
	switch importPath {
	case "os", "os/exec", "io", "io/fs", "syscall", "runtime", "golang.org/x/sys/unix":
		return true
	case "path/filepath":
		return method == "Abs" || method == "EvalSymlinks" || method == "Glob" || method == "Walk" || method == "WalkDir"
	case "time":
		return method == "Now" || method == "After" || method == "Since" || method == "Until"
	}
	switch method {
	case "LookPath", "Output", "VerifyTuple", "StartACPProcess", "CaptureIdentity", "ObserveIdentity", "ReadFile", "ReadDir", "Stat", "Lstat":
		return true
	}
	return false
}

func TestConstructorPurityGuardFollowsForgedLocalHelper(t *testing.T) {
	sources := map[string][]byte{"forged.go": []byte(`package dsh
import "os"
func NewPeerDriver() { constructorHelper() }
func constructorHelper() { _, _ = os.Stat("/tmp/forged") }
`)}
	violations, err := analyzeConstructorPurity(sources, []string{"NewPeerDriver"}, map[string]bool{"NewPeerDriver": true, "constructorHelper": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "constructorHelper reaches live call os.Stat") {
		t.Fatalf("forged helper violations = %v, want transitive os.Stat rejection", violations)
	}
}

func TestConstructorPurityGuardRejectsForgedReceiverHelper(t *testing.T) {
	sources := map[string][]byte{"forged.go": []byte(`package dsh
import "os"
type constructorProbe struct{}
func NewPeerDriver() { constructorProbe{}.snapshot() }
func (constructorProbe) snapshot() { _, _ = os.Stat("/tmp/forged") }
`)}
	violations, err := analyzeConstructorPurity(sources, []string{"NewPeerDriver"}, map[string]bool{"NewPeerDriver": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "NewPeerDriver reaches unreviewed receiver method snapshot") {
		t.Fatalf("receiver-helper violations = %v, want conservative receiver rejection", violations)
	}
}
