package releaseevidence

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/sessionbus/internal/releasepkg"
)

const (
	testCommit = "1111111111111111111111111111111111111111"
	testTree   = "2222222222222222222222222222222222222222"
	testRunID  = int64(123456)
)

func TestCanonicalizeValidatesSchemaAndProducesOneCanonicalLine(t *testing.T) {
	root := t.TempDir()
	schema := filepath.Join(root, "schema.json")
	input := filepath.Join(root, "input.json")
	output := filepath.Join(root, "output.json")
	writeTestFile(t, schema, `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["a","z"],"properties":{"a":{"type":"integer"},"z":{"type":"string"}}}`)
	writeTestFile(t, input, "{\n  \"z\": \"value\", \"a\": 7\n}\n")
	if err := Canonicalize(schema, input, output); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{\"a\":7,\"z\":\"value\"}\n" {
		t.Fatalf("canonical JSON = %q", body)
	}
}

func TestCanonicalizeRejectsSchemaViolationAndSupportsRFC8785Unicode(t *testing.T) {
	root := t.TempDir()
	schema := filepath.Join(root, "schema.json")
	input := filepath.Join(root, "input.json")
	output := filepath.Join(root, "output.json")
	writeTestFile(t, schema, `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["value"],"properties":{"value":{"type":"string"}}}`)
	writeTestFile(t, input, `{"wrong":"field"}`)
	if err := Canonicalize(schema, input, output); err == nil {
		t.Fatal("schema-invalid evidence was canonicalized")
	}
	writeTestFile(t, input, "{\"value\":\"Euro-€-\\u2028-line\"}")
	if err := Canonicalize(schema, input, output); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{\"value\":\"Euro-€- -line\"}\n" {
		t.Fatalf("RFC 8785 Unicode canonicalization = %q", body)
	}
}

func TestCrossCheckAcceptsExactArchivesAndRejectsBoundaryDrift(t *testing.T) {
	root := t.TempDir()
	archiveDir := filepath.Join(root, "archives")
	gateDir := filepath.Join(root, "gates")
	if err := os.Mkdir(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(gateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	document := testEvidenceDocument(t, root, archiveDir, gateDir)
	schema := filepath.Join("..", "..", "specs", "002-unified-user-daemon", "contracts", "release-evidence.schema.json")
	input := filepath.Join(root, "input.json")
	packageDir := filepath.Join(root, "packages")
	canonical := filepath.Join(root, "agent-sessions-v0.4.0-release-evidence.json")
	writeJSONTestFile(t, input, document)
	if err := Canonicalize(schema, input, canonical); err != nil {
		t.Fatal(err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, testCommit, testTree, testRunID); err != nil {
		t.Fatalf("exact release evidence failed: %v", err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, "3333333333333333333333333333333333333333", testTree, testRunID); err == nil {
		t.Fatal("changed release commit escaped cross-check")
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, testCommit, "3333333333333333333333333333333333333333", testRunID); err == nil {
		t.Fatal("changed release tree escaped cross-check")
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, testCommit, testTree, testRunID+1); err == nil {
		t.Fatal("changed workflow run escaped cross-check")
	}

	body, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical := filepath.Join(root, "noncanonical.json")
	writeTestFile(t, nonCanonical, " \n"+string(body))
	if err := CrossCheck(schema, nonCanonical, archiveDir, packageDir, gateDir, testCommit, testTree, testRunID); err == nil {
		t.Fatal("non-canonical evidence bytes escaped cross-check")
	}
	gateArtifact := filepath.Join(gateDir, "linux-normal.txt")
	originalGate, err := os.ReadFile(gateArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gateArtifact, append(originalGate, []byte("changed\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, testCommit, testTree, testRunID); err == nil {
		t.Fatal("changed gate evidence escaped cross-check")
	}
	if err := os.WriteFile(gateArtifact, originalGate, 0o600); err != nil {
		t.Fatal(err)
	}
	packageArtifact := filepath.Join(packageDir, "agent-sessions-dsh-comms-0.4.0.tgz")
	originalPackage, err := os.ReadFile(packageArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packageArtifact, append(originalPackage, []byte("changed")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, testCommit, testTree, testRunID); err == nil {
		t.Fatal("changed npm package bytes escaped cross-check")
	}
	if err := os.WriteFile(packageArtifact, originalPackage, 0o600); err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(packageDir, "unexpected-0.4.0.tgz")
	if err := os.WriteFile(unexpected, originalPackage, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, testCommit, testTree, testRunID); err == nil {
		t.Fatal("unexpected npm package artifact escaped cross-check")
	}
	if err := os.Remove(unexpected); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(archiveDir, "agent-sessions-0.4.0-linux-x64.tar.gz")
	file, err := os.OpenFile(archive, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("changed")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, packageDir, gateDir, testCommit, testTree, testRunID); err == nil {
		t.Fatal("changed archive bytes escaped cross-check")
	}
}

func TestGenerateConsumesAuthoritativeInventoryAndGateArtifacts(t *testing.T) {
	root := t.TempDir()
	archiveDir, gateDir := filepath.Join(root, "archives"), filepath.Join(root, "gates")
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	document := testEvidenceDocument(t, root, archiveDir, gateDir)
	inventoryPath, platformsPath := filepath.Join(root, "inventory.json"), filepath.Join(root, "platforms.txt")
	writeJSONTestFile(t, inventoryPath, document["package_inventory"])
	writeTestFile(t, platformsPath, "linux|amd64|linux-x64\nlinux|arm64|linux-arm64\ndarwin|amd64|darwin-x64\ndarwin|arm64|darwin-arm64\n")
	toolchains := document["toolchains"].(map[string]any)
	clients := document["native_clients"].(map[string]any)
	gates := document["gates"].(map[string]any)
	linuxPath := filepath.Join(root, "linux.json")
	writeJSONTestFile(t, linuxPath, map[string]any{
		"toolchain": toolchains["linux"], "native_clients": clients["linux"], "gates": gates["linux"],
	})
	output := filepath.Join(root, "agent-sessions-v0.4.0-release-evidence.json")
	err := Generate(GenerateOptions{
		SchemaPath:    filepath.Join("..", "..", "specs", "002-unified-user-daemon", "contracts", "release-evidence.schema.json"),
		InventoryPath: inventoryPath, PlatformsPath: platformsPath, ArchiveDir: archiveDir,
		PackageDir: filepath.Join(root, "packages"), GateDir: gateDir,
		LinuxGatePath: linuxPath, OutputPath: output, Version: "0.4.0",
		Commit: testCommit, Tree: testTree, RunID: testRunID, RunAttempt: 1,
		RunURL: "https://github.com/antst/sessionbus/actions/runs/123456",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' || bytes.Count(body, []byte{'\n'}) != 1 {
		t.Fatalf("generated evidence is not one canonical line: %q", body)
	}
}

func testEvidenceDocument(t *testing.T, root, archiveDir, gateDir string) map[string]any {
	t.Helper()
	executables := []string{"agent-sessions", "agent-sessions-hub"}
	plugins := []map[string]any{
		{"product": "codex", "plugin_id": "agent-sessions", "archive_paths": []string{".agents", ".codex-plugin", "hooks", "scripts", "skills"}},
		{"product": "claude", "plugin_id": "agent-sessions", "archive_paths": []string{".claude-plugin", "claude"}},
		{"product": "grok", "plugin_id": "agent-sessions", "archive_paths": []string{"grok"}},
		{"product": "qwen", "plugin_id": "agent-sessions", "archive_paths": []string{"qwen"}},
		{"product": "opencode", "plugin_id": "agent-sessions", "archive_paths": []string{"integrations/opencode"}},
		{"product": "kilo", "plugin_id": "agent-sessions", "archive_paths": []string{"integrations/kilo"}},
		{"product": "pi", "plugin_id": "agent-sessions", "archive_paths": []string{"integrations/pi"}},
		{"product": "omp", "plugin_id": "agent-sessions", "archive_paths": []string{"integrations/omp"}},
	}
	npmInventory := []map[string]any{
		{"path": "integrations/dsh/comms", "name": "@agent-sessions/dsh-comms"},
		{"path": "integrations/dsh/lane", "name": "@agent-sessions/dsh-lane"},
	}
	packageDir := filepath.Join(root, "packages")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	npmArtifacts := make([]any, 0, len(npmInventory))
	for _, npmPackage := range npmInventory {
		name := npmPackage["name"].(string)
		filename := strings.TrimPrefix(strings.ReplaceAll(name, "/", "-"), "@") + "-0.4.0.tgz"
		path := filepath.Join(packageDir, filename)
		writeNPMPackageFixture(t, path, name, "0.4.0")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		npmArtifacts = append(npmArtifacts, map[string]any{
			"path": npmPackage["path"], "name": name, "version": "0.4.0", "filename": filename,
			"byte_size": info.Size(), "sha256": digest, "source_commit": testCommit,
		})
	}
	archives := map[string]any{}
	for _, platform := range []string{"linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64"} {
		packageName := "agent-sessions-0.4.0-" + platform
		stage := filepath.Join(root, "stage-"+platform)
		packageRoot := filepath.Join(stage, packageName)
		for _, executable := range executables {
			path := filepath.Join(packageRoot, "bin", platform, executable)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(executable+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, plugin := range plugins {
			for _, archivePath := range plugin["archive_paths"].([]string) {
				path := filepath.Join(packageRoot, archivePath)
				if filepath.Ext(archivePath) != "" {
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.MkdirAll(path, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "fixture"), []byte("fixture\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		archivePath := filepath.Join(archiveDir, packageName+".tar.gz")
		if err := releasepkg.Create(stage, packageName, archivePath); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := fileSHA256(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		archives[platform] = map[string]any{
			"platform": platform, "filename": packageName + ".tar.gz", "byte_size": info.Size(),
			"sha256": digest, "source_commit": testCommit, "inventory_verified": true,
		}
	}
	gate := func(name string, index int) map[string]any {
		artifact := "linux-" + name + ".txt"
		body := []byte("linux " + name + " passed\n")
		if err := os.WriteFile(filepath.Join(gateDir, artifact), body, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		return map[string]any{
			"status":            "passed",
			"job_url":           "https://github.com/antst/sessionbus/actions/runs/123456/job/1" + string(rune('0'+index)),
			"evidence_artifact": artifact,
			"evidence_sha256":   hex.EncodeToString(digest[:]),
		}
	}
	gates := map[string]any{"linux": map[string]any{}}
	for index, name := range []string{"normal", "race", "vet", "lint", "focused_contracts", "quickstart", "permissions", "federation", "prebuilt_install", "owner_nonmutation"} {
		gates["linux"].(map[string]any)[name] = gate(name, index)
	}
	toolchain := map[string]any{"go": "go1.25.0", "golangci_lint": "2.12.2"}
	clients := map[string]any{"qwen": "0.22.0", "codex": "0.148.0", "claude": "2.1.237", "grok": "1.0.0"}
	return map[string]any{
		"schema_version": 1, "release_version": "0.4.0", "intended_tag": "v0.4.0",
		"commit_sha": testCommit, "tree_sha": testTree,
		"artifact": map[string]any{
			"file_name":              "agent-sessions-v0.4.0-release-evidence.json",
			"workflow_artifact_name": "agent-sessions-v0.4.0-release-evidence-" + testCommit,
			"retention_days":         90,
		},
		"workflow": map[string]any{
			"path": ".github/workflows/ci.yml", "run_id": testRunID, "run_attempt": 1,
			"run_url": "https://github.com/antst/sessionbus/actions/runs/123456",
		},
		"toolchains":     map[string]any{"linux": toolchain},
		"native_clients": map[string]any{"linux": clients},
		"gates":          gates, "archives": archives, "npm_packages": npmArtifacts,
		"package_inventory": map[string]any{
			"executables": executables, "plugin_payloads": plugins, "npm_packages": npmInventory,
		},
	}
}

func writeNPMPackageFixture(t *testing.T, path, name, version string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	body, err := json.Marshal(map[string]any{"name": name, "version": version})
	if err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "package/package.json", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeJSONTestFile(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
