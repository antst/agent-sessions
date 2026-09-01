package releaseinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type connectorCall struct {
	product Product
	args    []string
}

type recordingConnectorRunner struct {
	available map[Product]bool
	calls     []connectorCall
	fail      func(Product, []string) error
}

func (runner *recordingConnectorRunner) Available(product Product) bool {
	return runner.available[product]
}

func (runner *recordingConnectorRunner) Run(_ context.Context, product Product, args ...string) error {
	copyArgs := append([]string(nil), args...)
	runner.calls = append(runner.calls, connectorCall{product: product, args: copyArgs})
	if runner.fail != nil {
		return runner.fail(product, copyArgs)
	}
	return nil
}

func TestConnectorInstallUsesSupportedNativeInstallersAndExactSources(t *testing.T) {
	root := connectorSourceFixture(t)
	runner := &recordingConnectorRunner{available: allProductsAvailable()}
	transaction := ConnectorTransaction{SourceRoot: root, Runner: runner}

	if err := transaction.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []connectorCall{
		{CodexProduct, []string{"plugin", "marketplace", "add", root}},
		{CodexProduct, []string{"plugin", "add", "agent-sessions@agent-sessions"}},
		{ClaudeProduct, []string{"plugin", "marketplace", "add", "--scope", "user", root}},
		{ClaudeProduct, []string{"plugin", "install", "--scope", "user", "--yes", "agent-sessions@agent-sessions"}},
		{GrokProduct, []string{"plugin", "install", filepath.Join(root, "grok"), "--trust"}},
		{QwenProduct, []string{"extensions", "install", filepath.Join(root, "qwen"), "--consent", "--scope", "user"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("connector calls = %#v, want %#v", runner.calls, want)
	}
}

func TestConnectorInstallSkipsUnavailableOptionalProducts(t *testing.T) {
	root := connectorSourceFixture(t)
	runner := &recordingConnectorRunner{available: map[Product]bool{CodexProduct: true}}
	if err := (ConnectorTransaction{SourceRoot: root, Runner: runner}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if call.product != CodexProduct {
			t.Fatalf("optional unavailable product was invoked: %#v", call)
		}
	}
}

func TestConnectorInstallRollsBackPartialNativeMutations(t *testing.T) {
	root := connectorSourceFixture(t)
	runner := &recordingConnectorRunner{available: allProductsAvailable()}
	runner.fail = func(product Product, args []string) error {
		if product == GrokProduct && reflect.DeepEqual(args, []string{"plugin", "install", filepath.Join(root, "grok"), "--trust"}) {
			return errors.New("injected Grok install failure")
		}
		return nil
	}

	if err := (ConnectorTransaction{SourceRoot: root, Runner: runner}).Install(context.Background()); err == nil {
		t.Fatal("connector failure did not fail the transaction")
	}
	wantSuffix := []connectorCall{
		{GrokProduct, []string{"plugin", "uninstall", "agent-sessions", "--keep-data"}},
		{ClaudeProduct, []string{"plugin", "uninstall", "--scope", "user", "--keep-data", "agent-sessions@agent-sessions"}},
		{ClaudeProduct, []string{"plugin", "marketplace", "remove", "--scope", "user", "agent-sessions"}},
		{CodexProduct, []string{"plugin", "remove", "agent-sessions@agent-sessions"}},
		{CodexProduct, []string{"plugin", "marketplace", "remove", "agent-sessions"}},
	}
	if len(runner.calls) < len(wantSuffix) || !reflect.DeepEqual(runner.calls[len(runner.calls)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("rollback suffix = %#v, want %#v", runner.calls, wantSuffix)
	}
}

func TestConnectorRemovalUsesExactNativeRemovalForAllProducts(t *testing.T) {
	runner := &recordingConnectorRunner{available: allProductsAvailable()}
	if err := (ConnectorTransaction{SourceRoot: connectorSourceFixture(t), Runner: runner}).Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []connectorCall{
		{QwenProduct, []string{"extensions", "uninstall", "agent-sessions"}},
		{GrokProduct, []string{"plugin", "uninstall", "agent-sessions", "--keep-data"}},
		{ClaudeProduct, []string{"plugin", "uninstall", "--scope", "user", "--keep-data", "agent-sessions@agent-sessions"}},
		{ClaudeProduct, []string{"plugin", "marketplace", "remove", "--scope", "user", "agent-sessions"}},
		{CodexProduct, []string{"plugin", "remove", "agent-sessions@agent-sessions"}},
		{CodexProduct, []string{"plugin", "marketplace", "remove", "agent-sessions"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("removal calls = %#v, want %#v", runner.calls, want)
	}
}

func TestConnectorTransactionNeverReadsCredentialOrHistoryRoots(t *testing.T) {
	root := connectorSourceFixture(t)
	credentialCanaries := []string{
		filepath.Join(t.TempDir(), ".codex", "auth.json"),
		filepath.Join(t.TempDir(), ".claude", "credentials.json"),
		filepath.Join(t.TempDir(), ".grok", "history"),
		filepath.Join(t.TempDir(), ".qwen", "oauth_creds.json"),
	}
	allowed := map[string]bool{
		root: true, filepath.Join(root, "claude"): true,
		filepath.Join(root, "grok"): true, filepath.Join(root, "qwen"): true,
	}
	var inspected []string
	stat := func(path string) (os.FileInfo, error) {
		inspected = append(inspected, path)
		if !allowed[path] {
			for _, canary := range credentialCanaries {
				if path == canary || strings.HasPrefix(path, filepath.Dir(canary)+string(filepath.Separator)) {
					return nil, errors.New("credential canary was accessed")
				}
			}
			return nil, errors.New("unexpected path access")
		}
		return os.Stat(path)
	}
	runner := &recordingConnectorRunner{available: allProductsAvailable()}
	if err := (ConnectorTransaction{SourceRoot: root, Runner: runner, Stat: stat}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantInspected := []string{root, filepath.Join(root, "claude"), filepath.Join(root, "grok"), filepath.Join(root, "qwen")}
	if !reflect.DeepEqual(inspected, wantInspected) {
		t.Fatalf("inspected paths = %q, want only release sources %q", inspected, wantInspected)
	}
}

func connectorSourceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{"claude", "grok", "qwen"} {
		if err := os.Mkdir(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func allProductsAvailable() map[Product]bool {
	return map[Product]bool{CodexProduct: true, ClaudeProduct: true, GrokProduct: true, QwenProduct: true}
}
