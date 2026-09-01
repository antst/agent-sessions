package releasepkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCreateIsByteStableAndNormalizesArchiveMetadata(t *testing.T) {
	root := t.TempDir()
	packageName := "agent-sessions-v0.2.4-linux-x64"
	packageRoot := filepath.Join(root, packageName)
	if err := os.MkdirAll(filepath.Join(packageRoot, "bin", "linux-x64"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(packageRoot, "bin", "linux-x64", "qwen-peer")
	document := filepath.Join(packageRoot, "README.md")
	if err := os.WriteFile(executable, []byte("binary\n"), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(document, []byte("documentation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("qwen-peer", filepath.Join(packageRoot, "bin", "linux-x64", "peer-alias")); err != nil {
		t.Fatal(err)
	}

	first, second := filepath.Join(root, "first.tar.gz"), filepath.Join(root, "second.tar.gz")
	if err := Create(root, packageName, first); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(executable, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(document, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Create(root, packageName, second); err != nil {
		t.Fatal(err)
	}
	firstBody, _ := os.ReadFile(first)
	secondBody, _ := os.ReadFile(second)
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatal("equivalent release inputs produced different archive bytes")
	}

	entries := readArchiveEntries(t, first)
	wantNames := []string{
		packageName + "/",
		packageName + "/README.md",
		packageName + "/bin/",
		packageName + "/bin/linux-x64/",
		packageName + "/bin/linux-x64/peer-alias",
		packageName + "/bin/linux-x64/qwen-peer",
	}
	gotNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotNames = append(gotNames, entry.Name)
		if entry.Uid != 0 || entry.Gid != 0 || entry.Uname != "" || entry.Gname != "" || !entry.ModTime.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("archive metadata is not normalized for %s: %#v", entry.Name, entry)
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("archive entries = %q, want %q", gotNames, wantNames)
	}
	if entries[1].Mode != 0o644 || entries[4].Mode != 0o777 || entries[5].Mode != 0o755 {
		t.Fatalf("normalized modes = README %o alias %o executable %o", entries[1].Mode, entries[4].Mode, entries[5].Mode)
	}
}

func TestCreateRejectsUnsafePackageRoots(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"", ".", "../escape", "nested/package"} {
		if err := Create(root, name, filepath.Join(root, "out.tar.gz")); err == nil {
			t.Fatalf("unsafe package name %q was accepted", name)
		}
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "package")); err != nil {
		t.Fatal(err)
	}
	if err := Create(root, "package", filepath.Join(root, "out.tar.gz")); err == nil {
		t.Fatal("symlink package root was accepted")
	}
}

func readArchiveEntries(t *testing.T, path string) []*tar.Header {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	var result []*tar.Header
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			return result
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		copyHeader := *header
		result = append(result, &copyHeader)
	}
}
