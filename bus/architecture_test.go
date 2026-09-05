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

const modulePath = "github.com/antst/agent-sessions"

func TestBusAndWrappersKeepTheSplitReadyBoundary(t *testing.T) {
	repository := filepath.Clean("..")
	checkGoImports(t, filepath.Join(repository, "bus"), func(path, imported string) {
		if strings.HasPrefix(imported, modulePath+"/wrappers") {
			t.Errorf("%s imports wrapper package %s", path, imported)
		}
	})
	checkGoImports(t, filepath.Join(repository, "wrappers"), func(path, imported string) {
		if strings.HasPrefix(imported, modulePath+"/bus/") &&
			!strings.HasPrefix(imported, modulePath+"/bus/sdk/go") {
			t.Errorf("%s bypasses the public Go kit with %s", path, imported)
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

func checkGoImports(t *testing.T, root string, check func(string, string)) {
	t.Helper()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}
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
