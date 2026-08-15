package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRegisterAgyPluginManifestPreservesOtherImportsAndMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "import_manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{
  "format": "future",
  "imports": [
    {"name":"other-plugin","source":"marketplace","extra":{"kept":true}},
    {"name":"agent-sessions","source":"old","components":["skills"]}
  ]
}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RegisterAgyPluginManifest(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Format  string            `json:"format"`
		Imports []json.RawMessage `json:"imports"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != "future" || len(manifest.Imports) != 2 {
		t.Fatalf("manifest = %s", body)
	}
	var other map[string]any
	if err := json.Unmarshal(manifest.Imports[0], &other); err != nil {
		t.Fatal(err)
	}
	if other["name"] != "other-plugin" || other["extra"] == nil {
		t.Fatalf("other import was not preserved: %#v", other)
	}
	var installed agyPluginImport
	if err := json.Unmarshal(manifest.Imports[1], &installed); err != nil {
		t.Fatal(err)
	}
	if installed.Name != agyPluginName || installed.Source != "antigravity" || installed.ImportedAt == "" ||
		len(installed.Components) != 3 || installed.Components[1] != "mcpServers" {
		t.Fatalf("Agent Sessions import = %#v", installed)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %v, %v; want 0600", info, err)
	}
}

func TestRegisterAgyPluginManifestRejectsMalformedExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "import_manifest.json")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RegisterAgyPluginManifest(path); err == nil {
		t.Fatal("malformed existing manifest was overwritten")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "not json\n" {
		t.Fatalf("malformed manifest changed: %q, %v", body, err)
	}
}

func TestRegisterAgyPluginManifestRejectsNullExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "import_manifest.json")
	if err := os.WriteFile(path, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RegisterAgyPluginManifest(path); err == nil {
		t.Fatal("null existing manifest was accepted as an object")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "null\n" {
		t.Fatalf("null manifest changed: %q, %v", body, err)
	}
}
