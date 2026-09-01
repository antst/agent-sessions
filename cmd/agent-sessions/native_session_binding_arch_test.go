package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func TestLaneActorNativeSessionWritesStayAtReviewedBoundaries(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate command source directory")
	}
	sourceDir := filepath.Dir(sourceFile)
	path := filepath.Join(sourceDir, "lane.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// The actor receives the UUID from the product at start/open and thereafter
	// only follows that live value. Completion is deliberately absent: it can
	// corroborate the opened UUID but never rewrite it.
	allowed := reviewedLaneActorNativeSessionWrites()
	writes := laneActorNativeSessionWrites(parsed)
	sort.Strings(writes)
	allowedWrites := make([]string, 0, len(allowed))
	wantCount := 0
	for write, count := range allowed {
		allowedWrites = append(allowedWrites, fmt.Sprintf("%s (x%d)", write, count))
		wantCount += count
	}
	sort.Strings(allowedWrites)
	if len(writes) != wantCount {
		t.Fatalf("review every command native-session cache write; got %v, allowed %v", writes, allowedWrites)
	}
	actual := make(map[string]int, len(allowed))
	for _, write := range writes {
		actual[write]++
		if allowed[write] == 0 {
			t.Fatalf("unreviewed command native-session cache write %s", write)
		}
	}
	for write, count := range allowed {
		if actual[write] != count {
			t.Fatalf("reviewed command native-session cache write %s occurred %d times, want %d", write, actual[write], count)
		}
	}
}

func TestLaneActorNativeSessionGuardDetectsWholeStructForgery(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "forged.go", `package main
type laneActor struct { nativeID string }
func forged(actor *laneActor) { *actor = laneActor{nativeID: "forged"} }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	writes := laneActorNativeSessionWrites(parsed)
	reviewed := reviewedLaneActorNativeSessionWrites()
	want := map[string]bool{
		"forged:*actor=*ast.CompositeLit":         true,
		"forged:laneActor.nativeID=*ast.BasicLit": true,
	}
	for _, write := range writes {
		delete(want, write)
		if reviewed[write] != 0 {
			t.Fatalf("whole-struct forgery was accidentally reviewed: %s", write)
		}
	}
	if len(want) != 0 {
		t.Fatalf("whole-struct forgery escaped native-session inventory: writes=%v missing=%v", writes, want)
	}
}

func reviewedLaneActorNativeSessionWrites() map[string]int {
	return map[string]int{
		"startLane:actor.nativeID":            1,
		"recordLaneNativeID:actor.nativeID":   1,
		"copyLanePolicy:lane.NativeSessionID": 1,
	}
}

func laneActorNativeSessionWrites(parsed *ast.File) []string {
	writes := make([]string, 0)
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if ok {
				actorType, isActor := literal.Type.(*ast.Ident)
				if isActor && actorType.Name == "laneActor" {
					for _, element := range literal.Elts {
						field, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := field.Key.(*ast.Ident)
						if !ok || key.Name != "nativeID" {
							continue
						}
						value := fmt.Sprintf("%T", field.Value)
						if selector, ok := field.Value.(*ast.SelectorExpr); ok {
							if receiver, ok := selector.X.(*ast.Ident); ok {
								value = receiver.Name + "." + selector.Sel.Name
							}
						}
						writes = append(writes, fmt.Sprintf("%s:laneActor.nativeID=%s", function.Name.Name, value))
					}
				}
			}
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for index, left := range assignment.Lhs {
				if pointer, ok := left.(*ast.StarExpr); ok {
					if receiver, ok := pointer.X.(*ast.Ident); ok && receiver.Name == "actor" {
						value := "<missing>"
						if index < len(assignment.Rhs) {
							value = fmt.Sprintf("%T", assignment.Rhs[index])
							if identifier, ok := assignment.Rhs[index].(*ast.Ident); ok {
								value = identifier.Name
							}
						}
						writes = append(writes, fmt.Sprintf("%s:*actor=%s", function.Name.Name, value))
					}
				}
				selector, ok := left.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "nativeID" && selector.Sel.Name != "NativeSessionID" {
					continue
				}
				receiver, ok := selector.X.(*ast.Ident)
				if !ok {
					writes = append(writes, fmt.Sprintf("%s:%T.%s", function.Name.Name, selector.X, selector.Sel.Name))
					continue
				}
				writes = append(writes, fmt.Sprintf("%s:%s.%s", function.Name.Name, receiver.Name, selector.Sel.Name))
			}
			return true
		})
	}
	return writes
}
