package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestInstallHostAliasesSelectsOneStableHostImageAndRestoresPriorState(t *testing.T) {
	prefix := t.TempDir()
	binRoot := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	priorTarget := filepath.Join(prefix, "prior", "peer")
	if err := os.Symlink(priorTarget, filepath.Join(binRoot, "peer")); err != nil {
		t.Fatal(err)
	}
	restore, err := installHostAliases(prefix)
	if err != nil {
		t.Fatal(err)
	}
	wantTarget := filepath.Join(prefix, "libexec", "agent-sessions", "host", "current", "bin", "agent-sessions")
	for _, name := range append([]string{"agent-sessions"}, productcatalog.Catalog().HostAliases...) {
		target, readErr := os.Readlink(filepath.Join(binRoot, name))
		if readErr != nil || target != wantTarget {
			t.Errorf("host alias %s = %q, %v; want %q", name, target, readErr, wantTarget)
		}
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(binRoot, "peer")); err != nil || target != priorTarget {
		t.Fatalf("prior peer alias = %q, %v; want %q", target, err, priorTarget)
	}
	for _, name := range append([]string{"agent-sessions"}, productcatalog.Catalog().HostAliases...) {
		if name == "peer" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(binRoot, name)); !os.IsNotExist(err) {
			t.Errorf("rollback retained new alias %s: %v", name, err)
		}
	}
}

func TestInstallHostServiceDefinitionRendersSelectedPrefixAndRollsBack(t *testing.T) {
	home := t.TempDir()
	configuration := filepath.Join(home, "configuration")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configuration)
	source := t.TempDir()
	relative := filepath.Join("deploy", "agent-sessions", "systemd", "user", "agent-sessions.service")
	if runtime.GOOS == "darwin" {
		relative = filepath.Join("deploy", "agent-sessions", "launchd", "net.antst.agent-sessions.plist")
	}
	asset := filepath.Join(source, relative)
	if err := os.MkdirAll(filepath.Dir(asset), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("program=@PREFIX@ state=@STATE_ROOT@"), 0o600); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "prefix")
	state := filepath.Join(home, "state")
	restore, err := installHostServiceDefinition(source, prefix, state)
	if err != nil {
		t.Fatal(err)
	}
	target := hostServiceDefinitionPath()
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(body); !strings.Contains(text, prefix) || !strings.Contains(text, state) || strings.Contains(text, "@PREFIX@") {
		t.Fatalf("rendered host service definition = %q", text)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("service rollback retained a new definition: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("repeated service rollback is not idempotent: %v", err)
	}
}
