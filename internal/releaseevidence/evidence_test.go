package releaseevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/releasepkg"
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
	schema := filepath.Join("..", "..", "specs", "001-qwen-support", "contracts", "release-evidence.schema.json")
	input := filepath.Join(root, "input.json")
	canonical := filepath.Join(root, "agent-sessions-v0.2.4-release-evidence.json")
	writeJSONTestFile(t, input, document)
	if err := Canonicalize(schema, input, canonical); err != nil {
		t.Fatal(err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, gateDir, testCommit, testTree, testRunID); err != nil {
		t.Fatalf("exact release evidence failed: %v", err)
	}
	if err := CrossCheck(schema, canonical, archiveDir, gateDir, "3333333333333333333333333333333333333333", testTree, testRunID); err == nil {
		t.Fatal("changed release commit escaped cross-check")
	}
	if err := CrossCheck(schema, canonical, archiveDir, gateDir, testCommit, testTree, testRunID+1); err == nil {
		t.Fatal("changed workflow run escaped cross-check")
	}

	body, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical := filepath.Join(root, "noncanonical.json")
	writeTestFile(t, nonCanonical, " \n"+string(body))
	if err := CrossCheck(schema, nonCanonical, archiveDir, gateDir, testCommit, testTree, testRunID); err == nil {
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
	if err := CrossCheck(schema, canonical, archiveDir, gateDir, testCommit, testTree, testRunID); err == nil {
		t.Fatal("changed gate evidence escaped cross-check")
	}
	if err := os.WriteFile(gateArtifact, originalGate, 0o600); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(archiveDir, "agent-sessions-0.2.4-linux-x64.tar.gz")
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
	if err := CrossCheck(schema, canonical, archiveDir, gateDir, testCommit, testTree, testRunID); err == nil {
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
	linuxPath, macosPath := filepath.Join(root, "linux.json"), filepath.Join(root, "macos.json")
	writeJSONTestFile(t, linuxPath, map[string]any{
		"toolchain": toolchains["linux"], "native_clients": clients["linux"], "gates": gates["linux"],
	})
	writeJSONTestFile(t, macosPath, map[string]any{
		"toolchain": toolchains["macos"], "native_clients": clients["macos"], "gates": gates["macos"],
	})
	output := filepath.Join(root, "agent-sessions-v0.2.4-release-evidence.json")
	err := Generate(GenerateOptions{
		SchemaPath:    filepath.Join("..", "..", "specs", "001-qwen-support", "contracts", "release-evidence.schema.json"),
		InventoryPath: inventoryPath, PlatformsPath: platformsPath, ArchiveDir: archiveDir, GateDir: gateDir,
		LinuxGatePath: linuxPath, MacOSGatePath: macosPath, OutputPath: output, Version: "0.2.4",
		Commit: testCommit, Tree: testTree, RunID: testRunID, RunAttempt: 1,
		RunURL: "https://github.com/antst/agent-sessions/actions/runs/123456",
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
	executables := []string{
		"agent-session-runtime", "peer", "codex-peer", "claude-peer", "grok-peer", "qwen-peer",
		"codex-peer-lane", "claude-peer-lane", "grok-peer-lane", "qwen-peer-lane", "peer-federator",
	}
	plugins := []map[string]any{
		{"product": "codex", "plugin_id": "agent-sessions", "archive_paths": []string{".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills"}},
		{"product": "claude", "plugin_id": "agent-sessions", "archive_paths": []string{".claude-plugin", "claude"}},
		{"product": "grok", "plugin_id": "agent-sessions", "archive_paths": []string{"grok"}},
		{"product": "qwen", "plugin_id": "agent-sessions", "archive_paths": []string{"qwen"}},
	}
	archives := map[string]any{}
	for _, platform := range []string{"linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64"} {
		packageName := "agent-sessions-0.2.4-" + platform
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
	gate := func(osName, name string, index int) map[string]any {
		artifact := osName + "-" + name + ".txt"
		body := []byte(osName + " " + name + " passed\n")
		if err := os.WriteFile(filepath.Join(gateDir, artifact), body, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		return map[string]any{
			"status":            "passed",
			"job_url":           "https://github.com/antst/agent-sessions/actions/runs/123456/job/" + map[string]string{"linux": "1", "macos": "2"}[osName] + string(rune('0'+index)),
			"evidence_artifact": artifact,
			"evidence_sha256":   hex.EncodeToString(digest[:]),
		}
	}
	gates := map[string]any{}
	for _, osName := range []string{"linux", "macos"} {
		set := map[string]any{}
		for index, name := range []string{"normal", "race", "vet", "lint", "focused_contracts", "quickstart", "permissions", "federation", "prebuilt_install", "owner_nonmutation"} {
			set[name] = gate(osName, name, index)
		}
		gates[osName] = set
	}
	toolchain := map[string]any{"go": "go1.25.0", "golangci_lint": "2.12.2"}
	clients := map[string]any{"qwen": "0.22.0", "codex": "0.148.0", "claude": "2.1.237", "grok": "1.0.0"}
	return map[string]any{
		"schema_version": 1, "release_version": "0.2.4", "intended_tag": "v0.2.4",
		"commit_sha": testCommit, "tree_sha": testTree,
		"artifact": map[string]any{
			"file_name":              "agent-sessions-v0.2.4-release-evidence.json",
			"workflow_artifact_name": "agent-sessions-v0.2.4-release-evidence-" + testCommit,
			"retention_days":         90,
		},
		"workflow": map[string]any{
			"path": ".github/workflows/ci.yml", "run_id": testRunID, "run_attempt": 1,
			"run_url": "https://github.com/antst/agent-sessions/actions/runs/123456",
		},
		"toolchains":     map[string]any{"linux": toolchain, "macos": toolchain},
		"native_clients": map[string]any{"linux": clients, "macos": clients},
		"gates":          gates, "archives": archives,
		"package_inventory": map[string]any{"executables": executables, "plugin_payloads": plugins},
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
