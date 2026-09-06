package releaseevidence

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/antst/sessionbus/internal/productcatalog"
	"github.com/antst/sessionbus/internal/releasepkg"
)

func TestReleaseInventoryAliasesAndArchiveImagesAreExact(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(repository, "scripts", "release-inventory")
	hostAliases := inventoryLines(t, inventory, "host-aliases")
	var wantHostAliases []string
	for _, product := range productcatalog.All() {
		if product.PeerAlias != "" {
			wantHostAliases = append(wantHostAliases, product.PeerAlias)
		}
	}
	for _, product := range productcatalog.All() {
		if product.LaneAlias != "" {
			wantHostAliases = append(wantHostAliases, product.LaneAlias)
		}
	}
	if !reflect.DeepEqual(hostAliases, wantHostAliases) {
		t.Fatalf("host aliases = %q, want %q", hostAliases, wantHostAliases)
	}

	binaryRoot := t.TempDir()
	for _, platform := range releasepkg.SupportedPlatforms {
		platformRoot := filepath.Join(binaryRoot, platform.Name)
		if err := os.MkdirAll(platformRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, executable := range releasepkg.ExecutableNames {
			path := filepath.Join(platformRoot, executable)
			if err := os.WriteFile(path, []byte(platform.Name+":"+executable+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	artifacts, err := releasepkg.BuildArchives(releasepkg.BuildOptions{
		Version:      "0.3.0",
		SourceRoot:   repository,
		BinaryRoot:   binaryRoot,
		OutputRoot:   t.TempDir(),
		PayloadPaths: inventoryLines(t, inventory, "package-paths"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != len(releasepkg.SupportedPlatforms) || len(artifacts) != 4 {
		t.Fatalf("release artifacts = %d, want exactly four", len(artifacts))
	}
	for _, artifact := range artifacts {
		got := inventoryArchiveImages(t, artifact.Path, artifact.Platform.Name)
		want := append([]string(nil), releasepkg.ExecutableNames...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s executable images = %q, want %q", artifact.Path, got, want)
		}
	}
}

func inventoryLines(t *testing.T, inventory, operation string) []string {
	t.Helper()
	output, err := exec.Command(inventory, operation).Output()
	if err != nil {
		t.Fatalf("release inventory %s: %v", operation, err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("release inventory %s returned no rows", operation)
	}
	return lines
}

func inventoryArchiveImages(t *testing.T, archive, platform string) []string {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compressed.Close() }()
	reader := tar.NewReader(compressed)
	var images []string
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if strings.HasSuffix(name, "/scripts/cleanup-pre-unification") ||
			strings.HasSuffix(name, "/specs/002-unified-user-daemon/contracts/pre-unification-cleanup.yml") {
			t.Fatalf("repository-only cleanup artifact leaked into %s: %s", archive, name)
		}
		marker := "/bin/" + platform + "/"
		if index := strings.Index(name, marker); index >= 0 && !strings.Contains(name[index+len(marker):], "/") {
			if header.Typeflag != tar.TypeReg || header.Mode&0o111 == 0 {
				t.Fatalf("release image is not a regular executable: %s", name)
			}
			images = append(images, filepath.Base(name))
		}
	}
	sort.Strings(images)
	return images
}
