package bus_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/antst/sessionbus"

func TestBusAndWrappersKeepTheSplitReadyBoundary(t *testing.T) {
	repository := filepath.Clean("..")
	for _, required := range []string{filepath.Join(repository, "bus"), filepath.Join(repository, "wrappers", "README.md")} {
		if _, err := os.Stat(required); err != nil {
			t.Fatalf("required split-ready path %s: %v", required, err)
		}
	}
	checkGoImports(t, repository, func(path, imported string) {
		relative, err := filepath.Rel(repository, path)
		if err != nil {
			t.Fatal(err)
		}
		inBus := relative == "bus" || strings.HasPrefix(relative, "bus"+string(filepath.Separator))
		inWrappers := relative == "wrappers" || strings.HasPrefix(relative, "wrappers"+string(filepath.Separator))
		if inBus && strings.HasPrefix(imported, modulePath+"/wrappers") {
			t.Errorf("%s imports wrapper package %s", path, imported)
		}
		if !inBus && strings.HasPrefix(imported, modulePath+"/bus/internal/") {
			t.Errorf("%s imports private bus package %s", path, imported)
		}
		if inWrappers && strings.HasPrefix(imported, modulePath+"/bus/") &&
			!strings.HasPrefix(imported, modulePath+"/bus/sdk/go") {
			t.Errorf("%s bypasses the public Go kit with %s", path, imported)
		}
	})
	t.Run("wrapper imports", func(t *testing.T) {
		sources := 0
		if err := filepath.WalkDir(filepath.Join(repository, "wrappers"), func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && filepath.Ext(path) == ".go" {
				sources++
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if sources == 0 {
			t.Skip("no wrapper sources yet")
		}
	})
}

func TestBusSourceContainsNoProductNames(t *testing.T) {
	forbidden := []string{
		"clau" + "de", "co" + "dex", "gr" + "ok", "qw" + "en",
		"da" + "shi", "d" + "sh", "open" + "code", "ki" + "lo",
		"p" + "i", "o" + "mp",
	}
	root := "."
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || path == "architecture_test.go" || filepath.Ext(path) == ".md" {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lower := bytes.ToLower(contents)
		for _, product := range forbidden {
			if containsToken(lower, []byte(product)) {
				t.Errorf("%s contains product token %q", path, product)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBusAndWrappersContainNoFormerBrand(t *testing.T) {
	repository := filepath.Clean("..")
	former := []byte("agent" + "bus")
	for _, root := range []string{filepath.Join(repository, "bus"), filepath.Join(repository, "wrappers")} {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			if bytes.Contains(bytes.ToLower([]byte(path)), former) {
				t.Errorf("former brand remains in path %s", path)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(bytes.ToLower(contents), former) {
				t.Errorf("former brand remains in %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func checkGoImports(t *testing.T, root string, check func(string, string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, specification := range file.Imports {
			value, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return err
			}
			check(path, value)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func containsToken(contents, token []byte) bool {
	for from := 0; ; {
		index := bytes.Index(contents[from:], token)
		if index < 0 {
			return false
		}
		index += from
		before := index == 0 || !productCharacter(contents[index-1])
		afterAt := index + len(token)
		after := afterAt == len(contents) || !productCharacter(contents[afterAt])
		if before && after {
			return true
		}
		from = index + 1
	}
}

func productCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-'
}
