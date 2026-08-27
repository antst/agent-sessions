package clihelp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGenerateCheckedCLIDocumentation provides the one explicit mechanical
// rewrite path for the checked generated reference. Ordinary test runs never
// mutate the worktree.
func TestGenerateCheckedCLIDocumentation(t *testing.T) {
	if os.Getenv("UPDATE_CLI_DOCS") != "1" {
		t.Skip("set UPDATE_CLI_DOCS=1 to regenerate docs/CLI.md")
	}
	body, err := RenderMarkdown()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "docs", "CLI.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
