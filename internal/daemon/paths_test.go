package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveProductionPathsUsesOnlyCanonicalUserRoots(t *testing.T) {
	home := t.TempDir()
	configurationBase := filepath.Join(home, "configuration")
	stateBase := filepath.Join(home, "state")
	runtimeBase := filepath.Join(home, "runtime")
	for _, root := range []string{configurationBase, stateBase, runtimeBase} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configurationBase)
	t.Setenv("XDG_STATE_HOME", stateBase)
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	t.Setenv("TMPDIR", filepath.Join(home, "ignored-tmp"))

	paths, err := ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigurationFile != filepath.Join(configurationBase, "agent-sessions", "config.json") {
		t.Fatalf("configuration file = %q", paths.ConfigurationFile)
	}
	if paths.StateRoot != filepath.Join(stateBase, "agent-sessions") {
		t.Fatalf("state root = %q", paths.StateRoot)
	}
	if runtime.GOOS == "linux" && paths.RuntimeRoot != filepath.Join(runtimeBase, "agent-sessions") {
		t.Fatalf("Linux runtime root = %q", paths.RuntimeRoot)
	}
	if filepath.Clean(paths.ControlEndpoint) != paths.ControlEndpoint || !filepath.IsAbs(paths.ControlEndpoint) {
		t.Fatalf("control endpoint is not canonical: %q", paths.ControlEndpoint)
	}
}

func TestResolveProductionPathsRejectsRelativeXDGRoots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	if _, err := ResolveProductionPaths(); err == nil {
		t.Fatal("relative XDG_CONFIG_HOME was accepted")
	}
}
