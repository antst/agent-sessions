package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/releaseinstall"
)

func TestDurableConnectorHooksRecoverFreshProcessCrashBoundaries(t *testing.T) {
	for _, test := range []struct {
		name          string
		crashPoint    string
		crashProduct  string
		nativeCrash   bool
		rollbackCrash bool
		finish        string
		wantTarget    bool
	}{
		{name: "after prepare rolls back", crashPoint: "prepared", finish: "rollback"},
		{name: "pointer ready commits", finish: "commit", wantTarget: true},
		{name: "midway commit resumes forward", crashPoint: "applied", crashProduct: "claude", finish: "commit", wantTarget: true},
		{name: "midway commit rolls back exact prior", crashPoint: "applied", crashProduct: "claude", finish: "rollback"},
		{name: "uncertain native step resumes forward", nativeCrash: true, finish: "commit", wantTarget: true},
		{name: "uncertain native step restores prior", nativeCrash: true, finish: "rollback"},
		{name: "all products applied before terminal restores prior", crashPoint: "applied", crashProduct: "qwen", finish: "rollback"},
		{name: "uncertain native undo resumes rollback", rollbackCrash: true, finish: "rollback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDurableConnectorFixture(t)
			hooks := fixture.newHooks(t, true)
			if test.crashPoint != "" {
				hooks.crashCheckpoint = func(point, product string) {
					if point == test.crashPoint && product == test.crashProduct {
						panic("injected connector process crash")
					}
				}
			}
			request := releaseinstall.InstallRequest{
				Version: "1.2.3", ContentIdentity: "sha256:" + strings.Repeat("a", 64),
				SourceRoot: fixture.sourceRoot, Executable: "agent-sessions",
			}
			if test.crashPoint == "prepared" {
				assertConnectorCrash(t, func() { _ = hooks.Prepare(context.Background(), request) })
			} else {
				if err := hooks.Prepare(context.Background(), request); err != nil {
					t.Fatal(err)
				}
				switch {
				case test.rollbackCrash:
					if err := hooks.Commit(context.Background()); err != nil {
						t.Fatal(err)
					}
					fixture.panicExecutable = "qwen"
					fixture.panicArguments = "extensions uninstall " + qwenConnectorName
					assertConnectorCrash(t, func() { _ = hooks.Rollback(context.Background()) })
				case test.nativeCrash:
					fixture.panicExecutable = "codex"
					fixture.panicArguments = "plugin marketplace add " + fixture.sourceRoot
					assertConnectorCrash(t, func() { _ = hooks.Commit(context.Background()) })
				case test.crashPoint == "applied":
					assertConnectorCrash(t, func() { _ = hooks.Commit(context.Background()) })
				}
			}

			// Reconstruct only from the durable journal and exact native
			// provenance. No prepared mutation or new-request driver survives.
			fresh := fixture.newHooks(t, false)
			var err error
			if test.finish == "commit" {
				err = fresh.Commit(context.Background())
			} else {
				err = fresh.Rollback(context.Background())
			}
			if err != nil {
				t.Fatalf("fresh-process %s: %v", test.finish, err)
			}
			fixture.assertState(t, test.wantTarget)
			record, err := loadConnectorRecoveryJournal(fixture.journalPath)
			if err != nil {
				t.Fatal(err)
			}
			wantPhase := connectorRecoveryRolledBack
			wantProgress := connectorProductUndone
			if test.wantTarget {
				wantPhase = connectorRecoveryCommitted
				wantProgress = connectorProductApplied
			}
			if record.Phase != wantPhase || len(record.Products) != 4 {
				t.Fatalf("terminal connector recovery journal = %+v", record)
			}
			for _, product := range record.Products {
				if !product.Prepared || product.Progress != wantProgress || product.Provenance.Product != product.Product {
					t.Errorf("terminal %s provenance = %+v", product.Product, product)
				}
			}
		})
	}
}

func TestDurableConnectorCommitNeverFalseCompletesWithoutExactProvenance(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(*testing.T, string)
	}{
		{name: "absent"},
		{name: "malformed", create: func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"schema_version":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "indirect", create: func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "outside-connectors.json")
			if err := os.WriteFile(target, []byte(`{"schema_version":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transactions", "connectors.json")
			if test.create != nil {
				test.create(t, path)
			}
			hooks, err := NewDurableHostInstallHooks(nil, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := hooks.Commit(context.Background()); err == nil {
				t.Fatal("durable connector commit false-completed without exact prepared provenance")
			}
		})
	}
}

func TestDurableConnectorJournalRecordsEveryUnavailableProductAsExactNoop(t *testing.T) {
	root := connectorReleaseFixture(t)
	runner := &fakeConnectorRunner{look: func(string) (string, error) { return "", os.ErrNotExist }}
	drivers, err := NewNativeConnectorDrivers(NativeConnectorOptions{
		Runner: runner, GrokUserPluginRoot: filepath.Join(t.TempDir(), "plugins", "agent-sessions"),
	})
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(t.TempDir(), "transactions", "connectors.json")
	hooks, err := NewDurableHostInstallHooks(drivers, journalPath)
	if err != nil {
		t.Fatal(err)
	}
	request := releaseinstall.InstallRequest{
		Version: "1.2.3", ContentIdentity: "sha256:" + strings.Repeat("c", 64),
		SourceRoot: root, Executable: "agent-sessions",
	}
	if err := hooks.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewDurableHostInstallHooks(nil, journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := loadConnectorRecoveryJournal(journalPath)
	if err != nil || record.Phase != connectorRecoveryCommitted || len(record.Products) != 4 {
		t.Fatalf("unavailable connector journal = %+v, %v", record, err)
	}
	for _, product := range record.Products {
		if product.Provenance.Available || product.TotalSteps != 0 || product.Progress != connectorProductApplied {
			t.Errorf("unavailable %s provenance = %+v", product.Product, product)
		}
	}
}

func TestRunHostInstallCLIRecoversCrashedConnectorsBeforeReadingIndependentNewSource(t *testing.T) {
	fixture := newDurableConnectorFixture(t)
	prepared := fixture.newHooks(t, true)
	request := releaseinstall.InstallRequest{
		Version: "1.2.3", ContentIdentity: "sha256:" + strings.Repeat("b", 64),
		SourceRoot: fixture.sourceRoot, Executable: "agent-sessions",
	}
	if err := prepared.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	// The original invocation source is not recovery authority. Only the
	// staged transaction and persisted connector provenance survive a crash.
	if err := os.RemoveAll(fixture.sourceRoot); err != nil {
		t.Fatal(err)
	}
	newSource := filepath.Join(t.TempDir(), "malformed-new-source")
	if err := os.WriteFile(newSource, []byte("not a release directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := fixture.newHooks(t, false)
	transaction := &connectorRecoveryHostTransaction{hooks: fresh}
	configured := 0
	err := recoverThenInstallHostSource(context.Background(), transaction, newSource, "2.0.0", func() error {
		configured++
		return nil
	})
	if err == nil {
		t.Fatal("malformed independent new request reported success")
	}
	if transaction.recoverCalls != 1 || transaction.installCalls != 0 || configured != 0 {
		t.Fatalf("host CLI ordering = recover %d install %d configure %d",
			transaction.recoverCalls, transaction.installCalls, configured)
	}
	record, loadErr := loadConnectorRecoveryJournal(fixture.journalPath)
	if loadErr != nil || record.Phase != connectorRecoveryRolledBack {
		t.Fatalf("prior crashed connector recovery = %+v, %v", record, loadErr)
	}
	fixture.assertState(t, false)
}

type connectorRecoveryHostTransaction struct {
	hooks        *HostInstallHooks
	recoverCalls int
	installCalls int
}

func (transaction *connectorRecoveryHostTransaction) Recover(ctx context.Context) error {
	transaction.recoverCalls++
	return transaction.hooks.Rollback(ctx)
}

func (transaction *connectorRecoveryHostTransaction) Install(
	context.Context,
	releaseinstall.InstallRequest,
) (releaseinstall.InstallResult, error) {
	transaction.installCalls++
	return releaseinstall.InstallResult{}, nil
}

func assertConnectorCrash(t *testing.T, operation func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("connector crash injection did not terminate the hook call")
		}
	}()
	operation()
}

type durableConnectorFixture struct {
	sourceRoot        string
	journalPath       string
	runner            *fakeConnectorRunner
	groKUserRoot      string
	qwenHome          string
	priorCodexSource  string
	priorClaudeSource string
	priorQwenSource   string
	codex             codexConnectorState
	claude            claudeConnectorState
	grokEnabled       bool
	panicExecutable   string
	panicArguments    string
}

func newDurableConnectorFixture(t *testing.T) *durableConnectorFixture {
	t.Helper()
	root := t.TempDir()
	qwenHome := filepath.Join(root, "qwen-home")
	runtimeRoot := filepath.Join(root, "qwen-runtime")
	for _, path := range []string{qwenHome, runtimeRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QWEN_HOME", qwenHome)
	t.Setenv("QWEN_RUNTIME_DIR", runtimeRoot)
	sourceRoot := connectorReleaseFixture(t)
	priorQwenRoot := connectorReleaseFixture(t)
	priorQwenSource := filepath.Join(priorQwenRoot, "qwen")
	installQwenTestConnector(t, qwenHome, priorQwenSource)
	priorCodexSource := filepath.Join(root, "prior-codex")
	priorClaudeSource := filepath.Join(root, "prior-claude")
	for _, path := range []string{priorCodexSource, priorClaudeSource} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	userRoot := filepath.Join(root, "grok-plugins", "agent-sessions")
	priorGrokRoot := connectorReleaseFixture(t)
	if err := replaceConnectorTree(filepath.Join(priorGrokRoot, "grok"), userRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userRoot, "prior.txt"), []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &durableConnectorFixture{
		sourceRoot: sourceRoot, journalPath: filepath.Join(root, "transactions", "connectors.json"),
		groKUserRoot: userRoot, qwenHome: qwenHome,
		priorCodexSource: priorCodexSource, priorClaudeSource: priorClaudeSource, priorQwenSource: priorQwenSource,
		codex:       codexConnectorState{marketplace: true, plugin: true, source: priorCodexSource},
		claude:      claudeConnectorState{marketplace: true, plugin: true, source: priorClaudeSource},
		grokEnabled: true,
	}
	fixture.runner = &fakeConnectorRunner{output: fixture.output, run: fixture.run}
	return fixture
}

func (fixture *durableConnectorFixture) newHooks(t *testing.T, withDrivers bool) *HostInstallHooks {
	t.Helper()
	var drivers map[string]ConnectorDriver
	if withDrivers {
		var err error
		drivers, err = NewNativeConnectorDrivers(NativeConnectorOptions{
			Runner: fixture.runner, CodexExecutable: "codex", ClaudeExecutable: "claude",
			GrokExecutable: "grok", QwenExecutable: "qwen", GrokUserPluginRoot: fixture.groKUserRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	hooks, err := NewDurableHostInstallHooks(drivers, fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	hooks.recoveryRunner = fixture.runner
	return hooks
}

func (fixture *durableConnectorFixture) output(executable string, args []string) ([]byte, error) {
	switch filepath.Base(executable) {
	case "codex":
		if reflect.DeepEqual(args, []string{"plugin", "marketplace", "list", "--json"}) {
			return json.Marshal(map[string]any{"marketplaces": codexMarketplaceRows(fixture.codex)})
		}
		return json.Marshal(map[string]any{"installed": codexPluginRows(fixture.codex)})
	case "claude":
		if reflect.DeepEqual(args, []string{"plugin", "marketplace", "list", "--json"}) {
			rows := []any{}
			if fixture.claude.marketplace {
				rows = append(rows, map[string]any{"name": claudeMarketplace, "path": fixture.claude.source})
			}
			return json.Marshal(rows)
		}
		rows := []any{}
		if fixture.claude.plugin {
			rows = append(rows, map[string]any{"id": claudePluginID, "scope": "user"})
		}
		return json.Marshal(rows)
	case "grok":
		if !reflect.DeepEqual(args, []string{"inspect", "--json"}) {
			return nil, errors.New("unexpected Grok inventory call")
		}
		plugins, servers := []any{}, []any{}
		if _, err := os.Stat(fixture.groKUserRoot); err == nil && fixture.grokEnabled {
			plugins = append(plugins, map[string]any{
				"name": grokConnectorName, "scope": "user", "path": fixture.groKUserRoot, "enabled": true,
				"provides": map[string]any{"skills": 2, "mcpServers": 1},
			})
			servers = append(servers, map[string]any{
				"name": "agent_sessions", "transport": "stdio",
				"target": filepath.Join(fixture.groKUserRoot, "scripts", "native-entry"),
				"source": map[string]any{"type": "plugin", "plugin_name": grokConnectorName, "path": fixture.groKUserRoot},
			})
		}
		return json.Marshal(map[string]any{"plugins": plugins, "mcpServers": servers})
	default:
		return nil, errors.New("unexpected connector inventory executable")
	}
}

func (fixture *durableConnectorFixture) run(executable string, args []string) error {
	crashAfterMutation := filepath.Base(executable) == fixture.panicExecutable &&
		strings.Join(args, " ") == fixture.panicArguments
	if crashAfterMutation {
		fixture.panicExecutable, fixture.panicArguments = "", ""
		defer func() { panic("injected crash after native connector mutation") }()
	}
	switch filepath.Base(executable) {
	case "codex":
		switch strings.Join(args, " ") {
		case "plugin remove " + codexPluginID:
			fixture.codex.plugin = false
		case "plugin add " + codexPluginID:
			fixture.codex.plugin = true
		case "plugin marketplace remove " + codexMarketplace:
			fixture.codex.marketplace, fixture.codex.source = false, ""
		default:
			if len(args) == 4 && reflect.DeepEqual(args[:3], []string{"plugin", "marketplace", "add"}) {
				fixture.codex.marketplace, fixture.codex.source = true, args[3]
			} else {
				return errors.New("unexpected Codex command")
			}
		}
	case "claude":
		joined := strings.Join(args, " ")
		switch joined {
		case "plugin uninstall --scope user " + claudePluginID:
			fixture.claude.plugin = false
		case "plugin install --scope user " + claudePluginID:
			fixture.claude.plugin = true
		case "plugin marketplace remove --scope user " + claudeMarketplace:
			fixture.claude.marketplace, fixture.claude.source = false, ""
		default:
			if len(args) == 6 && reflect.DeepEqual(args[:5], []string{"plugin", "marketplace", "add", "--scope", "user"}) {
				fixture.claude.marketplace, fixture.claude.source = true, args[5]
			} else {
				return errors.New("unexpected Claude command: " + joined)
			}
		}
	case "grok":
		switch {
		case len(args) >= 3 && args[0] == "plugin" && args[1] == "install":
			fixture.grokEnabled = true
		case len(args) >= 3 && args[0] == "plugin" && args[1] == "uninstall":
			fixture.grokEnabled = len(args) == 4 && args[3] == "--keep-data"
		default:
			return errors.New("unexpected Grok command")
		}
	case "qwen":
		if reflect.DeepEqual(args, []string{"extensions", "uninstall", qwenConnectorName}) {
			return uninstallQwenTestConnector(fixture.qwenHome)
		}
		if len(args) == 6 && args[0] == "extensions" && args[1] == "install" {
			return installQwenTestConnectorError(fixture.qwenHome, args[2])
		}
		return errors.New("unexpected Qwen command")
	default:
		return errors.New("unexpected connector mutation executable")
	}
	return nil
}

func (fixture *durableConnectorFixture) assertState(t *testing.T, target bool) {
	t.Helper()
	wantCodex, wantClaude, wantQwen := fixture.priorCodexSource, fixture.priorClaudeSource, fixture.priorQwenSource
	if target {
		wantCodex, wantClaude, wantQwen = fixture.sourceRoot, fixture.sourceRoot, filepath.Join(fixture.sourceRoot, "qwen")
	}
	if !fixture.codex.marketplace || !fixture.codex.plugin || fixture.codex.source != wantCodex {
		t.Errorf("Codex recovery state = %+v, want source %q", fixture.codex, wantCodex)
	}
	if !fixture.claude.marketplace || !fixture.claude.plugin || fixture.claude.source != wantClaude {
		t.Errorf("Claude recovery state = %+v, want source %q", fixture.claude, wantClaude)
	}
	qwen, err := inspectQwenConnector(fixture.qwenHome)
	if err != nil || !qwen.present || !qwen.enabled || !sameConnectorSource(qwen.source, wantQwen) {
		t.Errorf("Qwen recovery state = %+v, %v; want source %q", qwen, err, wantQwen)
	}
	priorMarker := filepath.Join(fixture.groKUserRoot, "prior.txt")
	if target {
		if _, err := os.Lstat(priorMarker); !os.IsNotExist(err) {
			t.Errorf("target Grok recovery retained prior payload: %v", err)
		}
		if _, err := os.Stat(filepath.Join(fixture.groKUserRoot, ".mcp.json")); err != nil {
			t.Errorf("target Grok recovery lacks replacement payload: %v", err)
		}
	} else if body, err := os.ReadFile(priorMarker); err != nil || string(body) != "prior" {
		t.Errorf("prior Grok recovery = %q, %v", body, err)
	}
	if !fixture.grokEnabled {
		t.Error("Grok recovery did not retain enabled exact connector authority")
	}
}
