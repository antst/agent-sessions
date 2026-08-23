package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const (
	qwenTestPluginSchema  = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	qwenTestMCPSchema     = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	qwenTestPluginName    = "agent-sessions"
	qwenTestMCPName       = "agent_sessions"
	qwenTestPluginVersion = "0.2.1"
)

var qwenTestPluginSkills = []string{
	"agent-sessions",
	"claude-lane",
	"codex-lane",
	"grok-lane",
	"qwen-lane",
}

func TestQwenPluginPayloadMatchesAgentPluginsV1Contract(t *testing.T) {
	assertQwenPluginPayload(t, filepath.Join("..", "..", "qwen"))
}

func TestQwenPluginInstallCommandUsesOnlySelectedProfile(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "plugin")
	qwenExecutable := filepath.Join(root, "bin", "qwen")
	ambientHome := filepath.Join(root, "ambient-qwen")
	ambientRuntime := filepath.Join(root, "ambient-runtime")
	baseEnvironment := []string{
		"HOME=" + root,
		"QWEN_HOME=" + ambientHome,
		"QWEN_RUNTIME_DIR=" + ambientRuntime,
		"PRESERVE_QWEN_INSTALL_TEST=present",
	}
	wantArgs := []string{
		qwenExecutable, "extensions", "install", pluginRoot, "--scope", "user", "--consent",
	}

	defaultProfile := qwenTestProfileIdentity(t, map[string]string{"HOME": root})
	explicitHome := filepath.Join(root, "selected-qwen")
	explicitRuntime := filepath.Join(root, "selected-runtime")
	explicitProfile := qwenTestProfileIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": explicitHome, "QWEN_RUNTIME_DIR": explicitRuntime,
	})

	for _, test := range []struct {
		name        string
		profile     qwenprofile.Identity
		wantHome    string
		wantRuntime string
	}{
		{name: "native default remains unset", profile: defaultProfile},
		{name: "explicit profile is exact", profile: explicitProfile, wantHome: explicitHome, wantRuntime: explicitRuntime},
	} {
		t.Run(test.name, func(t *testing.T) {
			command, err := qwenPluginInstallCommand(qwenExecutable, pluginRoot, test.profile, baseEnvironment)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(command.Args, wantArgs) {
				t.Fatalf("Qwen install argv = %q, want %q", command.Args, wantArgs)
			}
			if got, present := qwenTestEnvironmentValue(command.Env, "QWEN_HOME"); present != test.profile.QwenHomeSet || got != test.wantHome {
				t.Fatalf("Qwen install QWEN_HOME = %q, present=%v", got, present)
			}
			if got, present := qwenTestEnvironmentValue(command.Env, "QWEN_RUNTIME_DIR"); present != test.profile.QwenRuntimeSet || got != test.wantRuntime {
				t.Fatalf("Qwen install QWEN_RUNTIME_DIR = %q, present=%v", got, present)
			}
			if got, present := qwenTestEnvironmentValue(command.Env, "PRESERVE_QWEN_INSTALL_TEST"); !present || got != "present" {
				t.Fatalf("unrelated install environment was not preserved: %q, present=%v", got, present)
			}
		})
	}
}

func TestVerifyQwenPluginInstallationRequiresExactEnabledInventory(t *testing.T) {
	if err := verifyQwenPluginInstallation(qwenTestPluginFixture(t, nil), qwenTestPluginVersion, true); err != nil {
		t.Fatalf("valid Qwen plugin installation rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(string)
		enabled bool
		want    string
	}{
		{name: "disabled", enabled: false, want: "enabled"},
		{name: "wrong version", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "plugin.json"), func(value map[string]any) { value["version"] = "0.2.0" })
		}, want: "version"},
		{name: "wrong plugin schema", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "plugin.json"), func(value map[string]any) { value["$schema"] = "https://example.invalid/plugin.schema.json" })
		}, want: "schema"},
		{name: "wrong MCP name", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "mcp.json"), func(value map[string]any) {
				servers := value["mcpServers"].(map[string]any)
				servers["wrong_name"] = servers[qwenTestMCPName]
				delete(servers, qwenTestMCPName)
			})
		}, want: qwenTestMCPName},
		{name: "missing skill", enabled: true, mutate: func(root string) {
			if err := os.Remove(filepath.Join(root, "skills", "qwen-lane", "SKILL.md")); err != nil {
				t.Fatal(err)
			}
		}, want: "skills"},
		{name: "extra skill", enabled: true, mutate: func(root string) {
			qwenTestWriteSkill(t, root, "unexpected")
		}, want: "skills"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := qwenTestPluginFixture(t, test.mutate)
			err := verifyQwenPluginInstallation(root, qwenTestPluginVersion, test.enabled)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("verification error = %v, want text %q", err, test.want)
			}
		})
	}
}

func assertQwenPluginPayload(t *testing.T, root string) {
	t.Helper()
	manifest := qwenTestReadJSONObject(t, filepath.Join(root, "plugin.json"))
	if manifest["$schema"] != qwenTestPluginSchema || manifest["name"] != qwenTestPluginName || manifest["version"] != qwenTestPluginVersion {
		t.Fatalf("Qwen plugin manifest identity = %#v", manifest)
	}
	allowedManifestKeys := map[string]bool{
		"$schema": true, "name": true, "version": true, "description": true, "author": true,
		"homepage": true, "repository": true, "license": true, "keywords": true, "extensions": true,
	}
	for key := range manifest {
		if !allowedManifestKeys[key] {
			t.Errorf("plugin.json contains Agent Plugins v1-unsupported key %q", key)
		}
	}

	mcp := qwenTestReadJSONObject(t, filepath.Join(root, "mcp.json"))
	servers, ok := mcp["mcpServers"].(map[string]any)
	if mcp["$schema"] != qwenTestMCPSchema || !ok || len(servers) != 1 {
		t.Fatalf("Qwen MCP manifest = %#v", mcp)
	}
	server, ok := servers[qwenTestMCPName].(map[string]any)
	if !ok || server["type"] != "stdio" || strings.TrimSpace(stringValue(server["command"])) == "" {
		t.Fatalf("Qwen %s MCP definition = %#v", qwenTestMCPName, servers[qwenTestMCPName])
	}

	skillsRoot := filepath.Join(root, "skills")
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	gotSkills := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Errorf("Qwen skills root contains non-directory %q", entry.Name())
			continue
		}
		info, err := os.Lstat(filepath.Join(skillsRoot, entry.Name(), "SKILL.md"))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("Qwen skill %q has no regular direct-child SKILL.md", entry.Name())
			continue
		}
		gotSkills = append(gotSkills, entry.Name())
	}
	sort.Strings(gotSkills)
	if !reflect.DeepEqual(gotSkills, qwenTestPluginSkills) {
		t.Errorf("Qwen plugin skills = %q, want %q", gotSkills, qwenTestPluginSkills)
	}
}

func qwenTestPluginFixture(t *testing.T, mutate func(string)) string {
	t.Helper()
	root := t.TempDir()
	qwenTestWriteJSON(t, filepath.Join(root, "plugin.json"), map[string]any{
		"$schema": qwenTestPluginSchema, "name": qwenTestPluginName, "version": qwenTestPluginVersion,
	})
	qwenTestWriteJSON(t, filepath.Join(root, "mcp.json"), map[string]any{
		"$schema": qwenTestMCPSchema,
		"mcpServers": map[string]any{
			qwenTestMCPName: map[string]any{"type": "stdio", "command": "agent-session-runtime", "args": []string{"qwen-mcp"}},
		},
	})
	for _, skill := range qwenTestPluginSkills {
		qwenTestWriteSkill(t, root, skill)
	}
	if mutate != nil {
		mutate(root)
	}
	return root
}

func qwenTestWriteSkill(t *testing.T, root, name string) {
	t.Helper()
	directory := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: Qwen plugin contract fixture.\n---\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func qwenTestWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func qwenTestReadJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func qwenTestRewriteJSONObject(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	value := qwenTestReadJSONObject(t, path)
	mutate(value)
	qwenTestWriteJSON(t, path, value)
}

func qwenTestProfileIdentity(t *testing.T, values map[string]string) qwenprofile.Identity {
	t.Helper()
	identity, err := qwenprofile.ResolveEnvironment(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func qwenTestEnvironmentValue(environment []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}
