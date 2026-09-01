package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackageUsesRepositoryReleaseImplementation(t *testing.T) {
	stage := t.TempDir()
	root := filepath.Join(stage, "agent-sessions-test")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "release.tar.gz")
	if err := run([]string{"package", stage, "agent-sessions-test", archive}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("archive stat = %v, %v", info, err)
	}
}

func TestEvidenceRejectsIncompleteArguments(t *testing.T) {
	if err := run([]string{"evidence", "generate", "--schema", "only"}); err == nil {
		t.Fatal("incomplete evidence arguments were accepted")
	}
}
