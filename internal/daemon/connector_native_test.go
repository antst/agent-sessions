package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

type fakeConnectorRunner struct {
	look   func(string) (string, error)
	output func(string, []string) ([]byte, error)
	run    func(string, []string) error
	calls  []string
}

func (runner *fakeConnectorRunner) LookPath(name string) (string, error) {
	if runner.look != nil {
		return runner.look(name)
	}
	return "/native/" + name, nil
}

func (runner *fakeConnectorRunner) Output(_ context.Context, executable string, args ...string) ([]byte, error) {
	if runner.output == nil {
		return nil, errors.New("unexpected inventory call")
	}
	return runner.output(executable, args)
}

func (runner *fakeConnectorRunner) Run(_ context.Context, executable string, args ...string) error {
	runner.calls = append(runner.calls, filepath.Base(executable)+" "+strings.Join(args, " "))
	if runner.run != nil {
		return runner.run(executable, args)
	}
	return nil
}

func TestNativeConnectorDriversTreatEveryMissingProductAsOptional(t *testing.T) {
	root := connectorReleaseFixture(t)
	for name, lookErr := range map[string]error{
		"bare command":  exec.ErrNotFound,
		"explicit path": &exec.Error{Name: filepath.Join(t.TempDir(), "missing-product"), Err: os.ErrNotExist},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeConnectorRunner{look: func(string) (string, error) { return "", lookErr }}
			drivers, err := NewNativeConnectorDrivers(NativeConnectorOptions{
				Runner: runner, GrokUserPluginRoot: filepath.Join(t.TempDir(), "plugins", "agent-sessions"),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, product := range productcatalog.Catalog().Products {
				mutation, err := drivers[product.ID].Prepare(context.Background(), ConnectorRequest{
					Product: product.ID, SourceRoot: root, Descriptor: product.Connector,
				})
				if err != nil {
					t.Errorf("%s missing product prepare: %v", product.ID, err)
					continue
				}
				if err := mutation.Commit(context.Background()); err != nil {
					t.Errorf("%s missing product commit: %v", product.ID, err)
				}
			}
		})
	}
}

func TestConnectorPayloadValidationCanonicalizesReleaseAndChildrenTogether(t *testing.T) {
	realRoot := connectorReleaseFixture(t)
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "release-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	descriptor, _ := productcatalog.ProductByID("claude")
	if err := validateConnectorRequest(ConnectorRequest{
		Product: "claude", SourceRoot: alias, Descriptor: descriptor.Connector,
	}, "claude", filepath.Join(alias, "claude")); err != nil {
		t.Fatalf("validate release through a canonicalizable alias: %v", err)
	}
}

func TestCodexConnectorMutationRestoresExactPriorMarketplace(t *testing.T) {
	root := connectorReleaseFixture(t)
	priorSource := filepath.Join(t.TempDir(), "prior")
	if err := os.MkdirAll(priorSource, 0o700); err != nil {
		t.Fatal(err)
	}
	state := codexConnectorState{marketplace: true, plugin: true, source: priorSource}
	runner := &fakeConnectorRunner{}
	runner.output = func(_ string, args []string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"plugin", "marketplace", "list", "--json"}) {
			return json.Marshal(map[string]any{"marketplaces": codexMarketplaceRows(state)})
		}
		return json.Marshal(map[string]any{"installed": codexPluginRows(state)})
	}
	runner.run = func(_ string, args []string) error {
		switch strings.Join(args, " ") {
		case "plugin remove " + codexPluginID:
			state.plugin = false
		case "plugin add " + codexPluginID:
			state.plugin = true
		case "plugin marketplace remove " + codexMarketplace:
			state.marketplace, state.source = false, ""
		case "plugin marketplace add " + root:
			state.marketplace, state.source = true, root
		case "plugin marketplace add " + priorSource:
			state.marketplace, state.source = true, priorSource
		default:
			return errors.New("unexpected Codex command")
		}
		return nil
	}
	driver := newCodexConnectorDriver(NativeConnectorOptions{Runner: runner, CodexExecutable: "codex"})
	descriptor, _ := productcatalog.ProductByID("codex")
	mutation, err := driver.Prepare(context.Background(), ConnectorRequest{Product: "codex", SourceRoot: root, Descriptor: descriptor.Connector})
	if err != nil {
		t.Fatal(err)
	}
	if err := mutation.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.source != root || !state.plugin {
		t.Fatalf("committed Codex connector = %+v", state)
	}
	if err := mutation.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state != (codexConnectorState{marketplace: true, plugin: true, source: priorSource}) {
		t.Fatalf("rolled back Codex connector = %+v", state)
	}
}

func TestClaudeConnectorMutationRestoresExactPriorMarketplace(t *testing.T) {
	root := connectorReleaseFixture(t)
	priorSource := filepath.Join(t.TempDir(), "prior")
	if err := os.MkdirAll(priorSource, 0o700); err != nil {
		t.Fatal(err)
	}
	state := claudeConnectorState{marketplace: true, plugin: true, source: priorSource}
	runner := &fakeConnectorRunner{}
	runner.output = func(_ string, args []string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"plugin", "marketplace", "list", "--json"}) {
			rows := []any{}
			if state.marketplace {
				rows = append(rows, map[string]any{"name": claudeMarketplace, "path": state.source})
			}
			return json.Marshal(rows)
		}
		rows := []any{}
		if state.plugin {
			rows = append(rows, map[string]any{"id": claudePluginID, "scope": "user"})
		}
		return json.Marshal(rows)
	}
	runner.run = func(_ string, args []string) error {
		joined := strings.Join(args, " ")
		switch joined {
		case "plugin uninstall --scope user " + claudePluginID:
			state.plugin = false
		case "plugin install --scope user " + claudePluginID:
			state.plugin = true
		case "plugin marketplace remove --scope user " + claudeMarketplace:
			state.marketplace, state.source = false, ""
		case "plugin marketplace add --scope user " + root:
			state.marketplace, state.source = true, root
		case "plugin marketplace add --scope user " + priorSource:
			state.marketplace, state.source = true, priorSource
		default:
			return errors.New("unexpected Claude command: " + joined)
		}
		return nil
	}
	driver := newClaudeConnectorDriver(NativeConnectorOptions{Runner: runner, ClaudeExecutable: "claude", ClaudeScope: "user"})
	descriptor, _ := productcatalog.ProductByID("claude")
	mutation, err := driver.Prepare(context.Background(), ConnectorRequest{Product: "claude", SourceRoot: root, Descriptor: descriptor.Connector})
	if err != nil {
		t.Fatal(err)
	}
	if err := mutation.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.source != root || !state.plugin {
		t.Fatalf("committed Claude connector = %+v", state)
	}
	if err := mutation.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state != (claudeConnectorState{marketplace: true, plugin: true, source: priorSource}) {
		t.Fatalf("rolled back Claude connector = %+v", state)
	}
}

func TestGrokConnectorMutationRestoresPriorPayloadWithoutCredentialAccess(t *testing.T) {
	root := connectorReleaseFixture(t)
	userRoot := filepath.Join(t.TempDir(), "plugins", "agent-sessions")
	if err := replaceConnectorTree(filepath.Join(root, "grok"), userRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userRoot, "prior.txt"), []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	runner := &fakeConnectorRunner{}
	runner.output = func(_ string, args []string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"inspect", "--json"}) {
			return nil, errors.New("unexpected Grok inventory call")
		}
		plugins := []any{}
		servers := []any{}
		if _, err := os.Stat(userRoot); err == nil && enabled {
			plugins = append(plugins, map[string]any{
				"name": grokConnectorName, "scope": "user", "path": userRoot, "enabled": true,
				"provides": map[string]any{"skills": 2, "mcpServers": 1},
			})
			servers = append(servers, map[string]any{
				"name": "agent_sessions", "transport": "stdio", "target": filepath.Join(userRoot, "scripts", "native-entry"),
				"source": map[string]any{"type": "plugin", "plugin_name": grokConnectorName, "path": userRoot},
			})
		}
		return json.Marshal(map[string]any{"plugins": plugins, "mcpServers": servers})
	}
	runner.run = func(_ string, args []string) error {
		if len(args) >= 3 && args[0] == "plugin" && args[1] == "install" {
			enabled = true
			return nil
		}
		if len(args) >= 3 && args[0] == "plugin" && args[1] == "uninstall" {
			enabled = len(args) == 4 && args[3] == "--keep-data"
			return nil
		}
		return errors.New("unexpected Grok command")
	}
	driver := newGrokConnectorDriver(NativeConnectorOptions{Runner: runner, GrokExecutable: "grok", GrokUserPluginRoot: userRoot})
	descriptor, _ := productcatalog.ProductByID("grok")
	mutation, err := driver.Prepare(context.Background(), ConnectorRequest{Product: "grok", SourceRoot: root, Descriptor: descriptor.Connector})
	if err != nil {
		t.Fatal(err)
	}
	if err := mutation.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(userRoot, ".mcp.json")); err != nil {
		t.Fatalf("replacement Grok payload missing: %v", err)
	}
	if err := mutation.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(userRoot, "prior.txt")); err != nil || string(body) != "prior" {
		t.Fatalf("prior Grok payload was not restored: body=%q err=%v", body, err)
	}
}

func TestGrokConnectorPrepareAcceptsInstalledProductWithoutPriorConnector(t *testing.T) {
	root := connectorReleaseFixture(t)
	immutableRoot := filepath.Join(root, "grok")
	if err := filepath.Walk(immutableRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = makeConnectorTreeWritable(immutableRoot) })
	userRoot := filepath.Join(t.TempDir(), "plugins", "agent-sessions")
	t.Cleanup(func() { _ = makeConnectorTreeWritable(userRoot) })
	enabled := false
	runner := &fakeConnectorRunner{}
	runner.output = func(_ string, args []string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"inspect", "--json"}) {
			return nil, errors.New("unexpected Grok inventory call")
		}
		plugins, servers := []any{}, []any{}
		if enabled {
			plugins = append(plugins, map[string]any{
				"name": grokConnectorName, "scope": "user", "path": userRoot, "enabled": true,
				"provides": map[string]any{"skills": 2, "mcpServers": 1},
			})
			servers = append(servers, map[string]any{
				"name": "agent_sessions", "transport": "stdio", "target": filepath.Join(userRoot, "scripts", "native-entry"),
				"source": map[string]any{"type": "plugin", "plugin_name": grokConnectorName, "path": userRoot},
			})
		}
		return json.Marshal(map[string]any{"plugins": plugins, "mcpServers": servers})
	}
	runner.run = func(_ string, args []string) error {
		if len(args) >= 3 && args[0] == "plugin" && args[1] == "install" {
			enabled = true
			return nil
		}
		if len(args) >= 3 && args[0] == "plugin" && args[1] == "uninstall" {
			enabled = len(args) == 4 && args[3] == "--keep-data"
			return nil
		}
		return errors.New("unexpected Grok command")
	}
	driver := newGrokConnectorDriver(NativeConnectorOptions{
		Runner: runner, GrokExecutable: "grok", GrokUserPluginRoot: userRoot,
	})
	descriptor, _ := productcatalog.ProductByID("grok")
	mutation, err := driver.Prepare(context.Background(), ConnectorRequest{
		Product: "grok", SourceRoot: root, Descriptor: descriptor.Connector,
	})
	if err != nil {
		t.Fatalf("prepare first Grok connector: %v", err)
	}
	if err := mutation.Commit(context.Background()); err != nil {
		t.Fatalf("commit first Grok connector: %v", err)
	}
}

func TestGrokConnectorInventoryIgnoresWorkspaceMCPRow(t *testing.T) {
	root := connectorReleaseFixture(t)
	userRoot := filepath.Join(root, "grok")
	workspaceServer := map[string]any{
		"name": "agent_sessions", "transport": "stdio", "target": filepath.Join(root, "scripts", "native-entry"),
		"source": map[string]any{"type": "mcpJson", "path": filepath.Join(root, ".mcp.json")},
	}
	pluginServer := map[string]any{
		"name": "agent_sessions", "transport": "stdio", "target": filepath.Join(userRoot, "scripts", "native-entry"),
		"source": map[string]any{"type": "plugin", "plugin_name": grokConnectorName, "path": userRoot},
	}
	body, err := json.Marshal(map[string]any{
		"plugins": []any{map[string]any{
			"name": grokConnectorName, "scope": "user", "path": userRoot, "enabled": true,
			"provides": map[string]any{"skills": 2, "mcpServers": 1},
		}},
		"mcpServers": []any{workspaceServer, pluginServer},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeConnectorRunner{output: func(_ string, args []string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"inspect", "--json"}) {
			return nil, errors.New("unexpected Grok inventory call")
		}
		return body, nil
	}}
	state, err := inspectGrokConnector(context.Background(), runner, "grok", userRoot)
	if err != nil || !state.present || !state.enabled {
		t.Fatalf("mixed workspace/plugin inventory = %+v, %v", state, err)
	}
	if err := VerifyGrokConnectorInventory(body, userRoot); err != nil {
		t.Fatalf("verify mixed workspace/plugin inventory: %v", err)
	}
}

func TestQwenConnectorMutationRestoresExactPriorSource(t *testing.T) {
	root := connectorReleaseFixture(t)
	home := filepath.Join(t.TempDir(), "qwen-home")
	runtimeRoot := filepath.Join(t.TempDir(), "qwen-runtime")
	for _, path := range []string{home, runtimeRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QWEN_HOME", home)
	t.Setenv("QWEN_RUNTIME_DIR", runtimeRoot)
	priorSourceRoot := connectorReleaseFixture(t)
	priorSource := filepath.Join(priorSourceRoot, "qwen")
	installQwenTestConnector(t, home, priorSource)
	runner := &fakeConnectorRunner{}
	runner.run = func(_ string, args []string) error {
		if reflect.DeepEqual(args, []string{"extensions", "uninstall", qwenConnectorName}) {
			return uninstallQwenTestConnector(home)
		}
		if len(args) == 6 && args[0] == "extensions" && args[1] == "install" {
			return installQwenTestConnectorError(home, args[2])
		}
		return errors.New("unexpected Qwen command: " + strings.Join(args, " "))
	}
	driver := newQwenConnectorDriver(NativeConnectorOptions{Runner: runner, QwenExecutable: "qwen"})
	descriptor, _ := productcatalog.ProductByID("qwen")
	mutation, err := driver.Prepare(context.Background(), ConnectorRequest{Product: "qwen", SourceRoot: root, Descriptor: descriptor.Connector})
	if err != nil {
		t.Fatal(err)
	}
	if err := mutation.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := inspectQwenConnector(home)
	if err != nil || !sameConnectorSource(current.source, filepath.Join(root, "qwen")) {
		t.Fatalf("committed Qwen connector = %+v, err=%v", current, err)
	}
	if err := mutation.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err = inspectQwenConnector(home)
	if err != nil || !sameConnectorSource(current.source, priorSource) {
		t.Fatalf("rolled back Qwen connector = %+v, err=%v", current, err)
	}
}

func installQwenTestConnector(t *testing.T, home, source string) {
	t.Helper()
	if err := installQwenTestConnectorError(home, source); err != nil {
		t.Fatal(err)
	}
}

func installQwenTestConnectorError(home, source string) error {
	root := filepath.Join(home, "extensions", qwenConnectorName)
	if err := replaceConnectorTree(source, root); err != nil {
		return err
	}
	installBody, _ := json.Marshal(map[string]any{"source": source, "type": "local"})
	if err := os.WriteFile(filepath.Join(root, ".qwen-extension-install.json"), installBody, 0o600); err != nil {
		return err
	}
	return writeQwenTestPolicy(home, true)
}

func uninstallQwenTestConnector(home string) error {
	if err := os.RemoveAll(filepath.Join(home, "extensions", qwenConnectorName)); err != nil {
		return err
	}
	return writeQwenTestPolicy(home, false)
}

func writeQwenTestPolicy(home string, present bool) error {
	extensions := map[string]any{}
	if present {
		extensions[qwenConnectorName] = map[string]any{"name": qwenConnectorName, "defaultActivation": "enabled"}
	}
	body, _ := json.Marshal(map[string]any{"version": 2, "extensions": extensions})
	path := filepath.Join(home, "extension-store", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func codexMarketplaceRows(state codexConnectorState) []any {
	if !state.marketplace {
		return nil
	}
	return []any{map[string]any{"name": codexMarketplace, "root": state.source}}
}

func codexPluginRows(state codexConnectorState) []any {
	if !state.plugin {
		return nil
	}
	return []any{map[string]any{"pluginId": codexPluginID}}
}

func connectorReleaseFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".mcp.json":                       "{}",
		"scripts/native-entry":            "#!/bin/sh\n",
		".claude-plugin/marketplace.json": "{}",
		"claude/.mcp.json":                "{}",
		"grok/.mcp.json":                  "{}",
		"grok/scripts/native-entry":       "#!/bin/sh\n",
		"qwen/mcp.json":                   "{}",
		"qwen/scripts/native-entry":       "#!/bin/sh\n",
		"qwen/plugin.json":                `{"name":"agent-sessions","version":"0.2.4"}`,
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
