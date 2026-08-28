package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/testutil"
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

func TestResolveInstalledProductExecutablesSkipsMissingOptionalProducts(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	resolved, err := resolveInstalledProductExecutables(map[string]string{
		"--codex":  executable,
		"--claude": missing,
		"--grok":   missing,
		"--qwen":   missing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved["codex"] != executable {
		t.Fatalf("resolved product executables = %#v, want only codex=%q", resolved, executable)
	}
}

func TestWriteInstalledProductConfigurationCreatesGreenfieldConfig(t *testing.T) {
	home := testutil.ShortSocketRoot(t, "asc-", filepath.Join("runtime", "agent-sessions", "daemon.sock"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "runtime"))
	paths, err := ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(home, "codex")
	if err := writeInstalledProductConfiguration(paths, map[string]string{"codex": executable}); err != nil {
		t.Fatal(err)
	}
	configuration, err := LoadDaemonConfig(paths.ConfigurationFile, paths)
	if err != nil {
		t.Fatal(err)
	}
	if got := configuration.ProductOverrides["codex"].Executable; got != executable {
		t.Fatalf("codex executable = %q, want %q", got, executable)
	}
}

func TestHostSurfaceRollbackProvenanceSurvivesProcessCrash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
	prefix := filepath.Join(home, "prefix")
	stateRoot := filepath.Join(home, "state")
	paths, err := ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigurationFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigurationFile, []byte("prior-configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	servicePath := hostServiceDefinitionPath()
	if err := os.MkdirAll(filepath.Dir(servicePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath, []byte("prior-service"), 0o640); err != nil {
		t.Fatal(err)
	}
	binRoot := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	priorPeer := filepath.Join(home, "prior-peer")
	if err := os.Symlink(priorPeer, filepath.Join(binRoot, "peer")); err != nil {
		t.Fatal(err)
	}
	record, err := captureHostSurfaceRollback(prefix, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveHostSurfaceRollback(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath, []byte("candidate-service"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigurationFile, []byte("candidate-configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range hostAliasNames() {
		if err := replaceAlias(filepath.Join(binRoot, name), filepath.Join(prefix, "candidate")); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh hook value has no in-memory undo closures, as after exec/crash.
	fresh := &hostInstallRoleHooks{prefix: prefix, stateRoot: stateRoot}
	if err := fresh.rollbackSurface(); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(servicePath); err != nil || string(body) != "prior-service" {
		t.Fatalf("crash-restored service = %q, %v", body, err)
	}
	if body, err := os.ReadFile(paths.ConfigurationFile); err != nil || string(body) != "prior-configuration" {
		t.Fatalf("crash-restored configuration = %q, %v", body, err)
	}
	if target, err := os.Readlink(filepath.Join(binRoot, "peer")); err != nil || target != priorPeer {
		t.Fatalf("crash-restored peer alias = %q, %v", target, err)
	}
	for _, name := range hostAliasNames() {
		if name == "peer" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(binRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("crash rollback retained candidate alias %q: %v", name, err)
		}
	}
	if _, err := os.Lstat(hostSurfaceRollbackPath(prefix)); !os.IsNotExist(err) {
		t.Fatalf("completed crash rollback retained provenance: %v", err)
	}
}

func TestHostSurfaceRollbackRejectsIndirectProvenanceWithoutMutation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
	prefix := filepath.Join(home, "prefix")
	stateRoot := filepath.Join(home, "state")
	path := hostSurfaceRollbackPath(prefix)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "outside-record")
	if err := os.WriteFile(target, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	servicePath := hostServiceDefinitionPath()
	if err := os.MkdirAll(filepath.Dir(servicePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath, []byte("candidate-service"), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks := &hostInstallRoleHooks{prefix: prefix, stateRoot: stateRoot}
	if err := hooks.rollbackSurface(); err == nil {
		t.Fatal("indirect crash rollback provenance was accepted")
	}
	if body, err := os.ReadFile(servicePath); err != nil || string(body) != "candidate-service" {
		t.Fatalf("rejected indirect provenance mutated service = %q, %v", body, err)
	}
}
